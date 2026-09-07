package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/mr-tron/base58"
)

const (
	bigMacURL     = "https://cdn.economistdatateam.com/big-mac/data/big-mac-full-index-jul-26.csv"
	opFeedPublish = 7
)

// --- RPC ---

type rpcRequest struct {
	JSONRPC string      `json:"jsonrpc"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params"`
	ID      int         `json:"id"`
}

type rpcResponse struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func rpcCall(endpoint, method string, params interface{}) (json.RawMessage, error) {
	body, _ := json.Marshal(rpcRequest{JSONRPC: "2.0", Method: method, Params: params, ID: 1})
	resp, err := http.Post(endpoint, "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var r rpcResponse
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, err
	}
	if r.Error != nil {
		return nil, fmt.Errorf("rpc error %d: %s", r.Error.Code, r.Error.Message)
	}
	return r.Result, nil
}

// --- Chain types ---

type dynamicGlobalProps struct {
	HeadBlockNumber uint32 `json:"head_block_number"`
	HeadBlockID     string `json:"head_block_id"`
	Time            string `json:"time"`
}

type asset struct {
	Amount    int64
	Precision uint8
	Symbol    string
}

func (a asset) String() string {
	return fmt.Sprintf("%.*f %s", a.Precision, float64(a.Amount)/math.Pow10(int(a.Precision)), a.Symbol)
}

func (a asset) serialize() []byte {
	buf := make([]byte, 16)
	binary.LittleEndian.PutUint64(buf[0:8], uint64(a.Amount))
	buf[8] = a.Precision
	copy(buf[9:16], a.Symbol)
	return buf
}

// --- Serialization helpers ---

func encodeVarint(v uint64) []byte {
	var buf []byte
	for v >= 0x80 {
		buf = append(buf, byte(v&0x7f)|0x80)
		v >>= 7
	}
	return append(buf, byte(v))
}

func serializeString(s string) []byte {
	b := encodeVarint(uint64(len(s)))
	return append(b, s...)
}

// --- WIF key decoding ---

func decodeWIF(wif string) (*btcec.PrivateKey, error) {
	raw, err := base58.Decode(wif)
	if err != nil {
		return nil, fmt.Errorf("base58: %w", err)
	}
	if len(raw) < 37 {
		return nil, fmt.Errorf("WIF too short")
	}
	payload := raw[:len(raw)-4]
	checksum := raw[len(raw)-4:]
	h := sha256.Sum256(payload)
	h = sha256.Sum256(h[:])
	if !bytes.Equal(h[:4], checksum) {
		return nil, fmt.Errorf("bad WIF checksum")
	}
	keyBytes := payload[1:] // strip version byte 0x80
	if len(keyBytes) == 33 && keyBytes[32] == 0x01 {
		keyBytes = keyBytes[:32] // strip compression flag
	}
	key, _ := btcec.PrivKeyFromBytes(keyBytes)
	return key, nil
}

// --- Witness config ---

type witnessKey struct {
	name string
	key  *btcec.PrivateKey
}

// parseWitnesses pairs comma-separated witness names with comma-separated
// WIF keys by position.
func parseWitnesses(names, wifs string) ([]witnessKey, error) {
	nameList := splitList(names)
	wifList := splitList(wifs)
	if len(nameList) != len(wifList) {
		return nil, fmt.Errorf("%d witness names but %d WIF keys", len(nameList), len(wifList))
	}
	witnesses := make([]witnessKey, len(nameList))
	for i, name := range nameList {
		if name == "" {
			return nil, fmt.Errorf("witness #%d: empty name", i+1)
		}
		key, err := decodeWIF(wifList[i])
		if err != nil {
			return nil, fmt.Errorf("witness %s: bad WIF key: %w", name, err)
		}
		witnesses[i] = witnessKey{name: name, key: key}
	}
	return witnesses, nil
}

func splitList(s string) []string {
	parts := strings.Split(s, ",")
	for i, part := range parts {
		parts[i] = strings.TrimSpace(part)
	}
	return parts
}

// --- Transaction building & signing ---

func buildTransaction(witness string, base, quote asset, props *dynamicGlobalProps) ([]byte, map[string]interface{}, error) {
	blockID, err := hex.DecodeString(props.HeadBlockID)
	if err != nil {
		return nil, nil, err
	}
	refBlockNum := uint16(props.HeadBlockNumber & 0xFFFF)
	refBlockPrefix := binary.LittleEndian.Uint32(blockID[4:8])
	blockTime, _ := time.Parse("2006-01-02T15:04:05", props.Time)
	expiration := blockTime.Add(60 * time.Second)
	expirationUnix := uint32(expiration.Unix())

	// Serialize for signing
	var buf bytes.Buffer
	binary.Write(&buf, binary.LittleEndian, refBlockNum)
	binary.Write(&buf, binary.LittleEndian, refBlockPrefix)
	binary.Write(&buf, binary.LittleEndian, expirationUnix)
	buf.Write(encodeVarint(1))             // 1 operation
	buf.Write(encodeVarint(opFeedPublish)) // op type
	buf.Write(serializeString(witness))    // publisher
	buf.Write(base.serialize())            // exchange_rate.base
	buf.Write(quote.serialize())           // exchange_rate.quote
	buf.Write(encodeVarint(0))             // extensions

	// JSON representation for broadcast
	tx := map[string]interface{}{
		"ref_block_num":    refBlockNum,
		"ref_block_prefix": refBlockPrefix,
		"expiration":       expiration.Format("2006-01-02T15:04:05"),
		"operations": []interface{}{
			[]interface{}{"feed_publish", map[string]interface{}{
				"publisher": witness,
				"exchange_rate": map[string]interface{}{
					"base":  base.String(),
					"quote": quote.String(),
				},
			}},
		},
		"extensions": []interface{}{},
	}

	return buf.Bytes(), tx, nil
}

func isCanonical(sig []byte) bool {
	return !(sig[1]&0x80 != 0 ||
		(sig[1] == 0 && sig[2]&0x80 == 0) ||
		sig[33]&0x80 != 0 ||
		(sig[33] == 0 && sig[34]&0x80 == 0))
}

func signTransaction(chainID string, txBytes []byte, key *btcec.PrivateKey) (string, error) {
	chainIDBytes, err := hex.DecodeString(chainID)
	if err != nil {
		return "", fmt.Errorf("bad chain_id hex: %w", err)
	}

	// Try adjusting expiration (last 4 bytes of txBytes before ops) to get canonical sig
	for attempt := 0; attempt < 20; attempt++ {
		h := sha256.New()
		h.Write(chainIDBytes)
		h.Write(txBytes)
		digest := h.Sum(nil)

		sig := ecdsa.SignCompact(key, digest, true)
		if isCanonical(sig) {
			return hex.EncodeToString(sig), nil
		}

		// Bump expiration by 1 second to change the digest
		expOffset := 2 + 4 // ref_block_num(2) + ref_block_prefix(4)
		exp := binary.LittleEndian.Uint32(txBytes[expOffset : expOffset+4])
		binary.LittleEndian.PutUint32(txBytes[expOffset:expOffset+4], exp+1)
	}
	return "", fmt.Errorf("could not produce canonical signature after 20 attempts")
}

// --- Big Mac price ---

func fetchBigMacPrice() (float64, string, error) {
	resp, err := http.Get(bigMacURL)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()

	reader := csv.NewReader(resp.Body)
	header, err := reader.Read()
	if err != nil {
		return 0, "", err
	}

	col := make(map[string]int)
	for i, h := range header {
		col[h] = i
	}
	for _, required := range []string{"date", "name", "dollar_price"} {
		if _, ok := col[required]; !ok {
			return 0, "", fmt.Errorf("missing column %q", required)
		}
	}

	var bestDate string
	var bestPrice float64

	for {
		row, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return 0, "", err
		}
		if row[col["name"]] != "United States" {
			continue
		}
		date := row[col["date"]]
		if date > bestDate {
			p, err := strconv.ParseFloat(row[col["dollar_price"]], 64)
			if err != nil {
				continue
			}
			bestDate = date
			bestPrice = p
		}
	}

	if bestPrice == 0 {
		return 0, "", fmt.Errorf("US Big Mac price not found in CSV")
	}
	return bestPrice, bestDate, nil
}

// --- Publishing ---

// publishFeed builds, signs and broadcasts one feed_publish transaction for w.
func publishFeed(rpc, chainID string, w witnessKey, base, quote asset) error {
	props, err := rpcCall(rpc, "condenser_api.get_dynamic_global_properties", []interface{}{})
	if err != nil {
		return fmt.Errorf("get props: %w", err)
	}
	var dgp dynamicGlobalProps
	json.Unmarshal(props, &dgp)

	txBytes, txJSON, err := buildTransaction(w.name, base, quote, &dgp)
	if err != nil {
		return fmt.Errorf("build tx: %w", err)
	}

	sig, err := signTransaction(chainID, txBytes, w.key)
	if err != nil {
		return fmt.Errorf("sign: %w", err)
	}

	// Sync JSON expiration with potentially adjusted txBytes
	expOffset := 2 + 4
	exp := binary.LittleEndian.Uint32(txBytes[expOffset : expOffset+4])
	txJSON["expiration"] = time.Unix(int64(exp), 0).UTC().Format("2006-01-02T15:04:05")
	txJSON["signatures"] = []string{sig}

	_, err = rpcCall(rpc, "condenser_api.broadcast_transaction_synchronous", []interface{}{txJSON})
	if err != nil {
		return fmt.Errorf("broadcast: %w", err)
	}
	return nil
}

// publishAll publishes the feed for every witness, one transaction each, so a
// failure for one witness does not block the others.
func publishAll(rpc, chainID string, witnesses []witnessKey, base, quote asset) error {
	var errs []error
	for _, w := range witnesses {
		if err := publishFeed(rpc, chainID, w, base, quote); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", w.name, err))
			continue
		}
		log.Printf("Feed published by %s: 1.000 PXS = %s", w.name, quote.String())
	}
	return errors.Join(errs...)
}

// --- Main ---

func main() {
	witness := flag.String("witness", "initminer", "Witness account name(s), comma-separated")
	wif := flag.String("wif", "", "Witness active key(s) (WIF), comma-separated in the same order as --witness. Also reads WITNESS_WIF env.")
	rpc := flag.String("rpc", "https://pixagram.dev", "RPC endpoint")
	chainID := flag.String("chain-id", "", "Chain ID (hex). If empty, fetched from --rpc via database_api.get_config.")
	tokenPrice := flag.Float64("token-price", 0.12, "Price of 1 PIXA in USD")
	interval := flag.Duration("interval", 1*time.Hour, "Feed publish interval")
	once := flag.Bool("once", false, "Publish once and exit")
	flag.Parse()

	if *wif == "" {
		*wif = os.Getenv("WITNESS_WIF")
	}
	if *wif == "" {
		log.Fatal("provide --wif or set WITNESS_WIF env")
	}

	witnesses, err := parseWitnesses(*witness, *wif)
	if err != nil {
		log.Fatal(err)
	}

	if *chainID == "" {
		cfg, err := rpcCall(*rpc, "database_api.get_config", map[string]interface{}{})
		if err != nil {
			log.Fatalf("fetch chain_id: %v", err)
		}
		var parsed struct {
			HiveChainID string `json:"HIVE_CHAIN_ID"`
		}
		if err := json.Unmarshal(cfg, &parsed); err != nil || parsed.HiveChainID == "" {
			log.Fatalf("fetch chain_id: could not parse HIVE_CHAIN_ID from get_config")
		}
		*chainID = parsed.HiveChainID
		log.Printf("Auto-detected chain_id: %s", *chainID)
	}

	log.Printf("Big Mac Feed — witnesses=%s rpc=%s token=$%.4f interval=%s",
		*witness, *rpc, *tokenPrice, *interval)

	publish := func() error {
		bigMacUSD, asOf, err := fetchBigMacPrice()
		if err != nil {
			return fmt.Errorf("fetch price: %w", err)
		}

		// 1 PXS = 1 Big Mac
		// How many PIXA to buy 1 Big Mac?
		pixaPerBigMac := bigMacUSD / *tokenPrice

		base := asset{Amount: 1000, Precision: 3, Symbol: "PXS"}
		quote := asset{
			Amount:    int64(math.Round(pixaPerBigMac * 1000)),
			Precision: 3,
			Symbol:    "PIXA",
		}

		log.Printf("Big Mac = $%.2f (as of %s) → 1 PXS = %s",
			bigMacUSD, asOf, quote.String())

		return publishAll(*rpc, *chainID, witnesses, base, quote)
	}

	if err := publish(); err != nil {
		log.Printf("ERROR: %v", err)
		if *once {
			os.Exit(1)
		}
	}

	if *once {
		return
	}

	ticker := time.NewTicker(*interval)
	defer ticker.Stop()
	for range ticker.C {
		if err := publish(); err != nil {
			log.Printf("ERROR: %v", err)
		}
	}
}

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/mr-tron/base58"
)

// testWIF builds a valid uncompressed WIF (0x80 || key || checksum) for a
// deterministic private key derived from seed, and returns both.
func testWIF(t *testing.T, seed string) (string, *btcec.PrivateKey) {
	t.Helper()
	keyBytes := sha256.Sum256([]byte(seed))
	payload := append([]byte{0x80}, keyBytes[:]...)
	h := sha256.Sum256(payload)
	h = sha256.Sum256(h[:])
	key, _ := btcec.PrivKeyFromBytes(keyBytes[:])
	return base58.Encode(append(payload, h[:4]...)), key
}

func TestParseWitnessesAcceptsSingleWitness(t *testing.T) {
	wif, key := testWIF(t, "initminer")

	got, err := parseWitnesses("initminer", wif)
	if err != nil {
		t.Fatalf("parseWitnesses: %v", err)
	}
	if len(got) != 1 || got[0].name != "initminer" {
		t.Fatalf("got %+v, want one witness named initminer", got)
	}
	if !bytes.Equal(got[0].key.Serialize(), key.Serialize()) {
		t.Errorf("witness key does not match the decoded WIF")
	}
}

func TestParseWitnessesPairsNamesWithKeysByPosition(t *testing.T) {
	aliceWIF, aliceKey := testWIF(t, "alice")
	bobWIF, bobKey := testWIF(t, "bob")

	got, err := parseWitnesses("alice,bob", aliceWIF+","+bobWIF)
	if err != nil {
		t.Fatalf("parseWitnesses: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d witnesses, want 2", len(got))
	}
	if got[0].name != "alice" || !bytes.Equal(got[0].key.Serialize(), aliceKey.Serialize()) {
		t.Errorf("witness 0 = %q, want alice paired with alice's key", got[0].name)
	}
	if got[1].name != "bob" || !bytes.Equal(got[1].key.Serialize(), bobKey.Serialize()) {
		t.Errorf("witness 1 = %q, want bob paired with bob's key", got[1].name)
	}
}

func TestParseWitnessesTrimsWhitespace(t *testing.T) {
	aliceWIF, _ := testWIF(t, "alice")
	bobWIF, _ := testWIF(t, "bob")

	got, err := parseWitnesses(" alice , bob ", aliceWIF+" , "+bobWIF)
	if err != nil {
		t.Fatalf("parseWitnesses: %v", err)
	}
	if len(got) != 2 || got[0].name != "alice" || got[1].name != "bob" {
		t.Fatalf("got %+v, want alice and bob", got)
	}
}

func TestParseWitnessesRejectsCountMismatch(t *testing.T) {
	wif, _ := testWIF(t, "alice")

	_, err := parseWitnesses("alice,bob", wif)
	if err == nil {
		t.Fatal("expected error for 2 names and 1 key, got nil")
	}
}

func TestParseWitnessesRejectsEmptyName(t *testing.T) {
	aliceWIF, _ := testWIF(t, "alice")
	bobWIF, _ := testWIF(t, "bob")

	_, err := parseWitnesses("alice,", aliceWIF+","+bobWIF)
	if err == nil {
		t.Fatal("expected error for empty witness name, got nil")
	}
}

func TestParseWitnessesRejectsBadWIFNamingTheWitness(t *testing.T) {
	wif, _ := testWIF(t, "alice")

	_, err := parseWitnesses("alice,bob", wif+",notakey")
	if err == nil || !strings.Contains(err.Error(), "bob") {
		t.Fatalf("expected error naming bob, got %v", err)
	}
}

// fakeChain is a minimal JSON-RPC server that answers
// get_dynamic_global_properties with fixed values and records the publisher
// of every broadcast feed_publish, rejecting the one named in rejectPublisher.
type fakeChain struct {
	rejectPublisher string
	publishers      []string
}

func (f *fakeChain) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	switch req.Method {
	case "condenser_api.get_dynamic_global_properties":
		fmt.Fprint(w, `{"result":{"head_block_number":1000,"head_block_id":"000003e80123456789abcdef0123456789abcdef","time":"2026-09-04T00:00:00"}}`)
	case "condenser_api.broadcast_transaction_synchronous":
		var params []struct {
			Operations [][]json.RawMessage `json:"operations"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil || len(params) != 1 || len(params[0].Operations) != 1 {
			http.Error(w, "unexpected broadcast params", http.StatusBadRequest)
			return
		}
		var op struct {
			Publisher string `json:"publisher"`
		}
		json.Unmarshal(params[0].Operations[0][1], &op)
		f.publishers = append(f.publishers, op.Publisher)
		if op.Publisher == f.rejectPublisher {
			fmt.Fprint(w, `{"error":{"code":-32003,"message":"missing required active authority"}}`)
			return
		}
		fmt.Fprint(w, `{"result":{"id":"deadbeef","block_num":1001}}`)
	default:
		http.Error(w, "unexpected method "+req.Method, http.StatusBadRequest)
	}
}

var (
	testChainID = strings.Repeat("00", 32)
	testBase    = asset{Amount: 1000, Precision: 3, Symbol: "PXS"}
	testQuote   = asset{Amount: 100000, Precision: 3, Symbol: "PIXA"}
)

func testWitnesses(t *testing.T, names ...string) []witnessKey {
	t.Helper()
	witnesses := make([]witnessKey, len(names))
	for i, name := range names {
		_, key := testWIF(t, name)
		witnesses[i] = witnessKey{name: name, key: key}
	}
	return witnesses
}

func TestPublishAllBroadcastsOneTransactionPerWitness(t *testing.T) {
	chain := &fakeChain{}
	srv := httptest.NewServer(chain)
	defer srv.Close()

	err := publishAll(srv.URL, testChainID, testWitnesses(t, "alice", "bob"), testBase, testQuote)
	if err != nil {
		t.Fatalf("publishAll: %v", err)
	}
	if want := []string{"alice", "bob"}; !reflect.DeepEqual(chain.publishers, want) {
		t.Errorf("broadcast publishers = %v, want %v", chain.publishers, want)
	}
}

func TestPublishAllContinuesAfterOneWitnessFails(t *testing.T) {
	chain := &fakeChain{rejectPublisher: "alice"}
	srv := httptest.NewServer(chain)
	defer srv.Close()

	err := publishAll(srv.URL, testChainID, testWitnesses(t, "alice", "bob"), testBase, testQuote)
	if err == nil || !strings.Contains(err.Error(), "alice") || strings.Contains(err.Error(), "bob") {
		t.Fatalf("expected an error naming only alice, got %v", err)
	}
	if want := []string{"alice", "bob"}; !reflect.DeepEqual(chain.publishers, want) {
		t.Errorf("broadcast publishers = %v, want %v (bob must still publish)", chain.publishers, want)
	}
}

func TestIndexCountryNormalizesToBigMacIndexCodes(t *testing.T) {
	for in, want := range map[string]string{
		"US":   "USA",
		"usa":  "USA",
		" gb ": "GBR",
		"CH":   "CHE",
		"DE":   "EUZ", // euro area members are only listed as "Euro area"
		"SVK":  "EUZ",
		"BG":   "EUZ",
		"euz":  "EUZ",
	} {
		got, err := indexCountry(in)
		if err != nil || got != want {
			t.Errorf("indexCountry(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

func TestIndexCountryRejectsUnknownCode(t *testing.T) {
	for _, in := range []string{"", "XYZ", "Germany"} {
		if got, err := indexCountry(in); err == nil {
			t.Errorf("indexCountry(%q) = %q, want error", in, got)
		}
	}
}

// staticServer answers every request with status and body.
func staticServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDetectCountryMapsIPLocationToIndexCode(t *testing.T) {
	srv := staticServer(t, http.StatusOK, `{"city":"Bratislava","country":"SK","timezone":"Europe/Bratislava"}`)

	got, err := detectCountry(srv.URL)
	if err != nil || got != "EUZ" {
		t.Fatalf("detectCountry = %q, %v; want EUZ", got, err)
	}
}

func TestDetectCountryRejectsResponseWithoutCountry(t *testing.T) {
	srv := staticServer(t, http.StatusOK, `{"bogon":true}`)

	if got, err := detectCountry(srv.URL); err == nil {
		t.Fatalf("detectCountry = %q, want error", got)
	}
}

func TestDetectCountryReportsHTTPError(t *testing.T) {
	srv := staticServer(t, http.StatusTooManyRequests, `{"error":"rate limited"}`)

	if got, err := detectCountry(srv.URL); err == nil {
		t.Fatalf("detectCountry = %q, want error", got)
	}
}

const testBigMacCSV = `date,iso_a3,currency_code,name,local_price,dollar_ex,dollar_price
2022-01-01,RUS,RUB,Russia,135,74.7,1.81
2026-01-01,USA,USD,United States,6.01,1,6.01
2026-07-01,USA,USD,United States,6.22,1,6.22
2026-07-01,EUZ,EUR,Euro area,6.19,0.87439,7.07922094259998
`

func TestFetchBigMacPricesKeepsOnlyLatestSurvey(t *testing.T) {
	srv := staticServer(t, http.StatusOK, testBigMacCSV)

	got, err := fetchBigMacPrices(srv.URL)
	if err != nil {
		t.Fatalf("fetchBigMacPrices: %v", err)
	}
	want := map[string]bigMacPrice{
		"USA": {Name: "United States", Price: 6.22, Currency: "USD", Date: "2026-07-01"},
		"EUZ": {Name: "Euro area", Price: 6.19, Currency: "EUR", Date: "2026-07-01"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("prices = %+v, want %+v (Russia dropped from the latest survey)", got, want)
	}
}

var testPrices = map[string]bigMacPrice{
	"USA": {Name: "United States", Price: 6.22, Currency: "USD", Date: "2026-07-01"},
	"EUZ": {Name: "Euro area", Price: 6.19, Currency: "EUR", Date: "2026-07-01"},
}

func TestPickBigMacPriceUsesCountry(t *testing.T) {
	got, err := pickBigMacPrice(testPrices, "EUZ", true)
	if err != nil || got.Name != "Euro area" {
		t.Fatalf("pickBigMacPrice = %+v, %v; want Euro area", got, err)
	}
}

func TestPickBigMacPriceFallsBackToUSForDetectedCountry(t *testing.T) {
	got, err := pickBigMacPrice(testPrices, "RUS", true)
	if err != nil || got.Name != "United States" {
		t.Fatalf("pickBigMacPrice = %+v, %v; want United States fallback", got, err)
	}
}

func TestFetchBigMacPricesSkipsRowsWithoutUsableLocalPrice(t *testing.T) {
	srv := staticServer(t, http.StatusOK, `date,iso_a3,currency_code,name,local_price,dollar_ex,dollar_price
2026-07-01,USA,USD,United States,6.22,1,6.22
2026-07-01,XXX,,Nowhere,5,1,5
2026-07-01,YYY,YYY,Free lunch,0,1,0
`)

	got, err := fetchBigMacPrices(srv.URL)
	if err != nil {
		t.Fatalf("fetchBigMacPrices: %v", err)
	}
	if len(got) != 1 || got["USA"].Price != 6.22 {
		t.Errorf("prices = %+v, want only the US row", got)
	}
}

func TestFetchBigMacPricesReportsHTTPError(t *testing.T) {
	srv := staticServer(t, http.StatusNotFound, "not found")

	if _, err := fetchBigMacPrices(srv.URL); err == nil {
		t.Fatal("expected error for HTTP 404, got nil")
	}
}

func TestParsePriceSpecAcceptsCommonForms(t *testing.T) {
	for in, want := range map[string]bigMacPrice{
		"7.30 CHF": {Price: 7.3, Currency: "CHF"},
		"CHF 7.30": {Price: 7.3, Currency: "CHF"},
		"7,30 chf": {Price: 7.3, Currency: "CHF"},
		"500JPY":   {Price: 500, Currency: "JPY"},
	} {
		got, err := parsePriceSpec(in)
		if err != nil || got.Price != want.Price || got.Currency != want.Currency {
			t.Errorf("parsePriceSpec(%q) = %+v, %v; want %v %s", in, got, err, want.Price, want.Currency)
		}
	}
}

func TestParsePriceSpecRejectsMissingCurrencyOrAmount(t *testing.T) {
	for _, in := range []string{"7.30", "CHF", "0 CHF", "CHF 7.30 EUR", "seven CHF", ""} {
		if got, err := parsePriceSpec(in); err == nil {
			t.Errorf("parsePriceSpec(%q) = %+v, want error", in, got)
		}
	}
}

func TestPickBigMacPriceRejectsMissingCountryFromFlag(t *testing.T) {
	got, err := pickBigMacPrice(testPrices, "RUS", false)
	if err == nil || !strings.Contains(err.Error(), "RUS") {
		t.Fatalf("pickBigMacPrice = %+v, %v; want error naming RUS", got, err)
	}
}

func TestNetworkCallsTimeOut(t *testing.T) {
	// The server never answers; each call must give up instead of hanging.
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer srv.Close()
	defer close(release) // runs before srv.Close, which waits for handlers

	old := httpClient.Timeout
	httpClient.Timeout = 50 * time.Millisecond
	defer func() { httpClient.Timeout = old }()

	calls := map[string]func() error{
		"rpcCall":            func() error { _, err := rpcCall(srv.URL, "condenser_api.get_config", []interface{}{}); return err },
		"detectCountry":      func() error { _, err := detectCountry(srv.URL); return err },
		"fetchBigMacPrices":  func() error { _, err := fetchBigMacPrices(srv.URL); return err },
		"fetchCoinstoreBook": func() error { _, err := fetchCoinstoreBook(srv.URL+"/", "PIXAUSDT"); return err },
		"frankfurterFX":      func() error { _, _, err := frankfurterFX{srv.URL}.rate("USD", "CHF"); return err },
		"erAPIFX":            func() error { _, _, err := erAPIFX{srv.URL}.rate("USD", "ARS"); return err },
	}
	for name, call := range calls {
		done := make(chan error, 1)
		go func() { done <- call() }()
		select {
		case err := <-done:
			if err == nil {
				t.Errorf("%s: expected timeout error, got nil", name)
			}
		case <-time.After(5 * time.Second):
			t.Errorf("%s: still hanging after 5s", name)
		}
	}
}

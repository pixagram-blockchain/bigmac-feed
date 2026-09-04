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

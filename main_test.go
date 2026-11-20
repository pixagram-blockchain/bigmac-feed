package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/mr-tron/base58"
	"golang.org/x/text/currency"
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

func TestNormalizeCountryAcceptsAlpha2Alpha3AndTheEuroArea(t *testing.T) {
	for in, want := range map[string]string{
		"US":   "US",
		"usa":  "US",
		" gb ": "GB",
		"CH":   "CH",
		"CHE":  "CH",
		"DE":   "DE", // euro area members have their own rows
		"SVK":  "SK",
		"euz":  "EUZ",
	} {
		got, err := normalizeCountry(in)
		if err != nil || got != want {
			t.Errorf("normalizeCountry(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

func TestNormalizeCountryRejectsUnknownCode(t *testing.T) {
	for _, in := range []string{"", "XYZ", "Germany", "001"} {
		if got, err := normalizeCountry(in); err == nil {
			t.Errorf("normalizeCountry(%q) = %q, want error", in, got)
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

func TestDetectCountryReturnsTheCountryOfTheIP(t *testing.T) {
	srv := staticServer(t, http.StatusOK, `{"city":"Bratislava","country":"SK","timezone":"Europe/Bratislava"}`)

	got, err := detectCountry(srv.URL)
	if err != nil || got != "SK" {
		t.Fatalf("detectCountry = %q, %v; want SK", got, err)
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

const priceFileHeader = "country_code,currency_code,low,high,median,date,sources\n"

const testPriceCSV = priceFileHeader + `US,USD,5.69,6.89,6.29,2026-09-30,"two restaurants: https://example.org/a | https://example.org/b"
DE,EUR,5.99,8.39,6.99,2026-10-24,https://example.org/de
FR,EUR,6.50,6.50,6.50,2026-10-01,https://example.org/fr
JP,JPY,500,500,500,2026-02-25,https://example.org/jp
`

var testPrices = map[string]bigMacPrice{
	"US": {Name: "US", Price: 6.29, Low: 5.69, High: 6.89, Currency: "USD", Date: "2026-09-30"},
	"DE": {Name: "DE", Price: 6.99, Low: 5.99, High: 8.39, Currency: "EUR", Date: "2026-10-24"},
	"FR": {Name: "FR", Price: 6.5, Low: 6.5, High: 6.5, Currency: "EUR", Date: "2026-10-01"},
	"JP": {Name: "JP", Price: 500, Low: 500, High: 500, Currency: "JPY", Date: "2026-02-25"},
}

func TestFetchBigMacPricesReadsTheMedianOfEachCountry(t *testing.T) {
	srv := staticServer(t, http.StatusOK, testPriceCSV)

	got, err := fetchBigMacPrices(srv.URL)
	if err != nil {
		t.Fatalf("fetchBigMacPrices: %v", err)
	}
	if !reflect.DeepEqual(got, testPrices) {
		t.Errorf("prices = %+v, want %+v", got, testPrices)
	}
}

func TestParseBigMacPricesLeavesOutUnusableRows(t *testing.T) {
	got, err := parseBigMacPrices(strings.NewReader(priceFileHeader + `US,USD,5.69,6.89,6.29,2026-09-30,the only good row
Nowhere,USD,5,5,5,2026-01-01,unknown country
EUZ,EUR,6,6,6,2026-01-01,the euro area is computed and has no row of its own
CH,,7.2,7.2,7.2,2026-02-21,no currency
GB,GBP,0,0,0,2026-01-01,free lunch
FR,EUR,7.00,6.00,6.50,2026-10-01,low above median
IT,EUR,5,7,8,2026-10-01,median above high
ES,EUR,6,6,6,October 2026,date not YYYY-MM-DD
NL,EUR,six,6,6,2026-10-01,not a number
SE,SEK,69
US,USD,1,1,1,2026-01-01,second row for the same country
`))
	if err != nil {
		t.Fatalf("parseBigMacPrices: %v", err)
	}
	if len(got) != 1 || got["US"].Price != 6.29 {
		t.Errorf("prices = %+v, want only the first US row", got)
	}
}

func TestParseBigMacPricesFindsColumnsByNameAndAcceptsAlpha3(t *testing.T) {
	got, err := parseBigMacPrices(strings.NewReader(`sources,median,low,high,currency_code,country_code,date,note
x,7.2,7.1,7.5,chf,CHE,2026-02-21,an extra column
`))
	want := map[string]bigMacPrice{"CH": {Name: "CH", Price: 7.2, Low: 7.1, High: 7.5, Currency: "CHF", Date: "2026-02-21"}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("prices = %+v, %v; want %+v", got, err, want)
	}
}

func TestParseBigMacPricesRejectsAnotherKindOfFile(t *testing.T) {
	// The Economist's index, which the feed read until v1.3.0.
	_, err := parseBigMacPrices(strings.NewReader(`date,iso_a3,currency_code,name,local_price,dollar_ex,dollar_price
2026-07-01,USA,USD,United States,6.22,1,6.22
`))
	if err == nil || !strings.Contains(err.Error(), "country_code") {
		t.Fatalf("expected an error naming the missing country_code column, got %v", err)
	}
}

func TestParseBigMacPricesRejectsFileWithoutUsableRow(t *testing.T) {
	if got, err := parseBigMacPrices(strings.NewReader(priceFileHeader)); err == nil {
		t.Fatalf("prices = %+v, want error", got)
	}
}

func TestFetchBigMacPricesReportsHTTPError(t *testing.T) {
	srv := staticServer(t, http.StatusNotFound, "not found")

	if _, err := fetchBigMacPrices(srv.URL); err == nil {
		t.Fatal("expected error for HTTP 404, got nil")
	}
}

func TestPickBigMacPriceUsesTheCountrysOwnRow(t *testing.T) {
	got, err := pickBigMacPrice(testPrices, "DE")
	if err != nil || got != testPrices["DE"] {
		t.Fatalf("pickBigMacPrice = %+v, %v; want the DE row", got, err)
	}
}

func TestPickBigMacPriceHasNoStandInForAMissingCountry(t *testing.T) {
	// Slovakia is in the euro area and has no row: neither the euro area
	// median nor another country's price may be used in its place.
	got, err := pickBigMacPrice(testPrices, "SK")
	if err == nil || !strings.Contains(err.Error(), "SK") || !strings.Contains(err.Error(), "--bigmac-price") {
		t.Fatalf("pickBigMacPrice = %+v, %v; want an error naming SK and --bigmac-price", got, err)
	}
}

func TestPickBigMacPriceEuroAreaIsTheMedianOfItsCountries(t *testing.T) {
	got, err := pickBigMacPrice(testPrices, "EUZ")
	if err != nil {
		t.Fatalf("pickBigMacPrice: %v", err)
	}
	if got.Price != (6.99+6.5)/2 || got.Currency != "EUR" || got.Low != 5.99 || got.High != 8.39 {
		t.Errorf("euro area = %+v, want the median of DE and FR in EUR, from 5.99 to 8.39", got)
	}
	if got.Date != "2026-10-01" {
		t.Errorf("euro area date = %q, want the oldest of its rows, 2026-10-01", got.Date)
	}
	if !strings.Contains(got.Name, "DE, FR") {
		t.Errorf("euro area name = %q, want it to list DE, FR", got.Name)
	}
}

func TestPickBigMacPriceEuroAreaNeedsAEuroAreaRow(t *testing.T) {
	prices := map[string]bigMacPrice{"US": testPrices["US"], "JP": testPrices["JP"]}
	if got, err := pickBigMacPrice(prices, "EUZ"); err == nil {
		t.Fatalf("pickBigMacPrice = %+v, want error", got)
	}
}

func TestPriceSourceFallsBackToTheLastCopyFetched(t *testing.T) {
	down := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if down {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprint(w, testPriceCSV)
	}))
	defer srv.Close()
	source := &priceSource{url: srv.URL}

	got, origin, err := source.prices()
	if err != nil || origin != "" || !reflect.DeepEqual(got, testPrices) {
		t.Fatalf("prices = %+v, %q, %v; want the published file", got, origin, err)
	}

	down = true
	got, origin, err = source.prices()
	if err != nil || origin != "last copy fetched" || !reflect.DeepEqual(got, testPrices) {
		t.Fatalf("prices = %+v, %q, %v; want the last copy fetched", got, origin, err)
	}
}

func TestPriceSourceFallsBackToTheBuiltInCopy(t *testing.T) {
	srv := staticServer(t, http.StatusServiceUnavailable, "down")
	builtIn, err := parseBigMacPrices(bytes.NewReader(embeddedPrices))
	if err != nil {
		t.Fatalf("built-in price file: %v", err)
	}

	for name, url := range map[string]string{"published file out of reach": srv.URL, "no URL": ""} {
		got, origin, err := (&priceSource{url: url}).prices()
		if err != nil || origin != "copy built into this version" || !reflect.DeepEqual(got, builtIn) {
			t.Errorf("%s: prices = %+v, %q, %v; want the built-in copy", name, got, origin, err)
		}
	}
}

// checkPriceFile applies the rules of the README's "Price data" section to a
// price file and returns what breaks them. It is stricter than the feed's
// own reader, which leaves a bad row out instead of failing.
func checkPriceFile(data []byte) []string {
	var problems []string
	rows, err := csv.NewReader(bytes.NewReader(data)).ReadAll()
	if err != nil {
		return []string{err.Error()}
	}
	if len(rows) < 2 {
		return []string{"no rows"}
	}
	if got := strings.Join(rows[0], ","); got+"\n" != priceFileHeader {
		problems = append(problems, fmt.Sprintf("header is %q, want %q", got, strings.TrimSpace(priceFileHeader)))
	}
	previous := ""
	for i, row := range rows[1:] {
		line := i + 2
		if len(row) != 7 {
			problems = append(problems, fmt.Sprintf("line %d: %d fields, want 7", line, len(row)))
			continue
		}
		country, currencyCode, date, sources := row[0], row[1], row[5], row[6]
		if normalized, err := normalizeCountry(country); err != nil || normalized != country || len(country) != 2 {
			problems = append(problems, fmt.Sprintf("line %d: country_code %q is not an ISO 3166 alpha-2 code", line, country))
		}
		if country <= previous {
			problems = append(problems, fmt.Sprintf("line %d: %s must come after %s, one row per country in alphabetical order", line, country, previous))
		}
		previous = country
		if !isISOCurrency(currencyCode) {
			problems = append(problems, fmt.Sprintf("line %d: currency_code %q is not an ISO 4217 code", line, currencyCode))
		}
		low, high, median := parsePrice(row[2]), parsePrice(row[3]), parsePrice(row[4])
		if !(low > 0) || low > median || median > high || math.IsInf(high, 0) {
			problems = append(problems, fmt.Sprintf("line %d: need 0 < low <= median <= high, got %s, %s, %s", line, row[2], row[4], row[3]))
		}
		day, err := time.Parse("2006-01-02", date)
		if err != nil {
			problems = append(problems, fmt.Sprintf("line %d: date %q is not YYYY-MM-DD", line, date))
		} else if day.After(time.Now().Add(24 * time.Hour)) {
			problems = append(problems, fmt.Sprintf("line %d: date %s is in the future", line, date))
		}
		if strings.TrimSpace(sources) == "" {
			problems = append(problems, fmt.Sprintf("line %d: no source", line))
		}
	}
	if parsed, err := parseBigMacPrices(bytes.NewReader(data)); err != nil || len(parsed) != len(rows)-1 {
		problems = append(problems, fmt.Sprintf("the feed reads %d of the %d rows (%v)", len(parsed), len(rows)-1, err))
	}
	return problems
}

// newerISOCurrencies are current ISO 4217 codes that golang.org/x/text's
// table does not have yet, such as Venezuela's VES.
var newerISOCurrencies = map[string]bool{"MRU": true, "SLE": true, "VED": true, "VES": true, "ZWG": true}

func isISOCurrency(code string) bool {
	if newerISOCurrencies[code] {
		return true
	}
	unit, err := currency.ParseISO(code)
	return err == nil && unit.String() == code
}

func TestPriceFilesFollowTheRules(t *testing.T) {
	for _, name := range []string{"latest.csv", "delivery.csv"} {
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, problem := range checkPriceFile(data) {
			t.Errorf("%s: %s", name, problem)
		}
	}
}

func TestCheckPriceFileCatchesBrokenRows(t *testing.T) {
	problems := checkPriceFile([]byte(priceFileHeader + `DE,EUR,5.99,8.39,6.99,2026-10-24,https://example.org/de
CHE,CHF,7.2,7.2,7.2,2026-02-21,alpha-3 code
AT,EURO,6,6,6,2026-10-01,bad currency
FR,EUR,7,6,6.5,2026-10-01,low above median
JP,JPY,500,500,500,2999-01-01,date in the future
KR,KRW,5700,5700,5700,2026-02-20,
`))
	for _, want := range []string{"line 3: country_code", "line 4: currency_code", "line 4: AT must come after", "line 5: need 0 < low", "line 6: date 2999-01-01 is in the future", "line 7: no source"} {
		found := false
		for _, p := range problems {
			found = found || strings.Contains(p, want)
		}
		if !found {
			t.Errorf("missing problem %q in %q", want, problems)
		}
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

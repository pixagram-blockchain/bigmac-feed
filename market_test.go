package main

import (
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// handlerServer serves every request with h.
func handlerServer(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestFairValueOfDeepBookIsTheMid(t *testing.T) {
	book := newOrderBook(
		[]level{{0.119, 5000}, {0.118, 5000}},
		[]level{{0.121, 5000}, {0.122, 5000}},
	)
	f, err := book.fairValue(1000, 0.25)
	if err != nil {
		t.Fatalf("fairValue: %v", err)
	}
	if !near(f.Fair, 0.12) {
		t.Errorf("fair = %v, want mid 0.12", f.Fair)
	}
}

func TestFairValueIgnoresTheLastTrade(t *testing.T) {
	// The PIXA/USDT book on 30 Oct 2026, when the last trade was a 1-PIXA
	// sell at 0.0367: the fair value stays at the top of the book.
	book := newOrderBook(
		[]level{{0.1339, 5375.54}, {0.133, 751.87}, {0.1326, 4941.5}, {0.0401, 134.79}},
		[]level{{0.145, 2.99}, {0.246, 190.67}, {0.247, 54.2}, {0.4977, 39.72}, {0.5411, 103.89}, {0.598, 399.29}, {0.6, 39.73}, {0.898, 399.29}},
	)
	f, err := book.fairValue(1000, 0.25)
	if err != nil {
		t.Fatalf("fairValue: %v", err)
	}
	if f.Fair < f.BestBid || f.Fair > f.BestAsk {
		t.Errorf("fair = %v, want within the spread [%v, %v]", f.Fair, f.BestBid, f.BestAsk)
	}
	if !near(f.Fair, 0.145) {
		t.Errorf("fair = %v, want 0.145 (impact mid above the ask, bounded by it)", f.Fair)
	}
}

func TestFairValueIgnoresDustBehindTheTop(t *testing.T) {
	bids := []level{{0.119, 5000}}
	asks := []level{{0.121, 5000}}
	base, _ := newOrderBook(bids, asks).fairValue(1000, 0.25)

	// 1-PIXA orders far from the mid change nothing.
	dusty, err := newOrderBook(append(bids, level{0.01, 1}), append(asks, level{9.99, 1})).fairValue(1000, 0.25)
	if err != nil {
		t.Fatalf("fairValue: %v", err)
	}
	if dusty.Fair != base.Fair {
		t.Errorf("fair moved from %v to %v for dust far from the mid", base.Fair, dusty.Fair)
	}
}

func TestFairValueDustInsideTheSpreadMovesItOnlyWithinTheSpread(t *testing.T) {
	bids := []level{{0.119, 5000}}
	asks := []level{{0.121, 5000}}
	base, _ := newOrderBook(bids, asks).fairValue(1000, 0.25)

	// A 1-PIXA bid inside the spread is a real order and bounds the fair
	// value from below, but cannot take it past the deep book's spread.
	dusty, err := newOrderBook(append([]level{{0.1205, 1}}, bids...), asks).fairValue(1000, 0.25)
	if err != nil {
		t.Fatalf("fairValue: %v", err)
	}
	if dusty.Fair < 0.119 || dusty.Fair > 0.121 || math.Abs(dusty.Fair-base.Fair) > (0.121-0.119)/2 {
		t.Errorf("fair moved from %v to %v, beyond half the deep spread", base.Fair, dusty.Fair)
	}
}

func TestFairValueWithZeroImpactSizeIsTopOfBookMid(t *testing.T) {
	book := newOrderBook([]level{{0.10, 1}, {0.05, 10000}}, []level{{0.12, 1}})
	f, err := book.fairValue(0, 0.25)
	if err != nil {
		t.Fatalf("fairValue: %v", err)
	}
	if !near(f.Fair, 0.11) {
		t.Errorf("fair = %v, want 0.11", f.Fair)
	}
}

func TestFairValueSortsLevelsAndDropsBadOnes(t *testing.T) {
	book := newOrderBook(
		[]level{{0.10, 100}, {0.119, 5000}, {0, 50}, {0.15, 0}},
		[]level{{0.13, 100}, {0.121, 5000}, {-1, 5}},
	)
	f, err := book.fairValue(1000, 0.25)
	if err != nil {
		t.Fatalf("fairValue: %v", err)
	}
	if f.BestBid != 0.119 || f.BestAsk != 0.121 || !near(f.Fair, 0.12) {
		t.Errorf("got bid %v ask %v fair %v, want 0.119 / 0.121 / 0.12", f.BestBid, f.BestAsk, f.Fair)
	}
}

func TestFairValueRejectsUnusableBooks(t *testing.T) {
	for name, tc := range map[string]struct {
		bids, asks []level
	}{
		"no asks":    {[]level{{0.12, 100}}, nil},
		"no bids":    {nil, []level{{0.12, 100}}},
		"crossed":    {[]level{{0.13, 100}}, []level{{0.12, 100}}},
		"too wide":   {[]level{{0.10, 100}}, []level{{0.20, 100}}},
		"only dust":  {[]level{{0, 100}}, []level{{0.12, 0}}},
		"empty book": {nil, nil},
	} {
		if f, err := newOrderBook(tc.bids, tc.asks).fairValue(1000, 0.25); err == nil {
			t.Errorf("%s: fair = %v, want error", name, f.Fair)
		}
	}
}

func TestFairValueSpreadGuardCanBeDisabled(t *testing.T) {
	f, err := newOrderBook([]level{{0.10, 100}}, []level{{0.20, 100}}).fairValue(100, 0)
	if err != nil || !near(f.Fair, 0.15) {
		t.Fatalf("fair = %v, %v; want 0.15", f.Fair, err)
	}
}

func TestSweepAveragesAcrossLevels(t *testing.T) {
	avg, filled := sweep([]level{{1, 10}, {2, 10}}, 15)
	if !near(avg, 20.0/15) || filled != 15 {
		t.Errorf("sweep = %v, %v; want %v, 15", avg, filled, 20.0/15)
	}
	avg, filled = sweep([]level{{1, 10}}, 15)
	if avg != 1 || filled != 10 {
		t.Errorf("thin sweep = %v, %v; want 1, 10", avg, filled)
	}
}

func TestParsePairInfersQuoteCurrency(t *testing.T) {
	for spec, want := range map[string]coinstoreMarket{
		"coinstore:PIXAUSDT":     {symbol: "PIXAUSDT", quote: "USDT"},
		"coinstore:pixausd":      {symbol: "PIXAUSD", quote: "USD"},
		"Coinstore:PIXAEUR":      {symbol: "PIXAEUR", quote: "EUR"},
		"coinstore:PIXAXYZ:CHF ": {symbol: "PIXAXYZ", quote: "CHF"},
	} {
		m, err := parsePair(spec)
		if err != nil {
			t.Errorf("parsePair(%q): %v", spec, err)
			continue
		}
		c := m.(coinstoreMarket)
		if c.symbol != want.symbol || c.quote != want.quote {
			t.Errorf("parsePair(%q) = %s/%s, want %s/%s", spec, c.symbol, c.quote, want.symbol, want.quote)
		}
	}
}

func TestParsePairRejectsBadSpecs(t *testing.T) {
	for _, spec := range []string{"PIXAUSDT", "coinstore:", "coinstore:PIXAXYZ", "binance:PIXAUSDT"} {
		if _, err := parsePair(spec); err == nil {
			t.Errorf("parsePair(%q): want error", spec)
		}
	}
}

func TestFetchCoinstoreBookParsesDepth(t *testing.T) {
	var path string
	srv := handlerServer(t, func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.RequestURI()
		w.Write([]byte(`{"data":{"channel":"1972@depth@100","a":[["0.246","190.67",-1],["0.145","2.99",-1]],"b":[["0.1339","5375.54",1],[0.133,751.87,1]],"level":100,"symbol":"PIXAUSDT"},"code":0}`))
	})

	book, err := fetchCoinstoreBook(srv.URL+"/api/v1/market/depth/", "PIXAUSDT")
	if err != nil {
		t.Fatalf("fetchCoinstoreBook: %v", err)
	}
	if path != "/api/v1/market/depth/PIXAUSDT?depth=100" {
		t.Errorf("requested %q", path)
	}
	if len(book.Bids) != 2 || len(book.Asks) != 2 || book.Bids[0].Price != 0.1339 || book.Asks[0].Price != 0.145 || book.Bids[1].Qty != 751.87 {
		t.Errorf("book = %+v", book)
	}
}

func TestFetchCoinstoreBookReportsAPIError(t *testing.T) {
	srv := staticServer(t, http.StatusOK, `{"message":"Field symbol Invalid","code":400}`)

	_, err := fetchCoinstoreBook(srv.URL+"/", "NOPEUSDT")
	if err == nil || !strings.Contains(err.Error(), "Field symbol Invalid") {
		t.Fatalf("expected Coinstore error, got %v", err)
	}
}

func TestCoinstoreMarketNamesItselfInErrors(t *testing.T) {
	srv := staticServer(t, http.StatusOK, `{"data":{"a":[],"b":[["0.12","10",1]]},"code":0}`)

	m := coinstoreMarket{symbol: "PIXAUSDT", quote: "USDT", baseURL: srv.URL + "/"}
	_, err := m.fairQuote(1000, 0.25)
	if err == nil || !strings.Contains(err.Error(), "PIXAUSDT") || !strings.Contains(err.Error(), "one-sided") {
		t.Fatalf("expected one-sided book error naming PIXAUSDT, got %v", err)
	}
}

func TestMedian(t *testing.T) {
	if m := median([]float64{3, 1, 2}); m != 2 {
		t.Errorf("median odd = %v", m)
	}
	if m := median([]float64{4, 1, 2, 3}); m != 2.5 {
		t.Errorf("median even = %v", m)
	}
	if !math.IsNaN(median(nil)) {
		t.Error("median of nothing should be NaN")
	}
}

package main

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func testFX(rates map[string]float64) fxSource {
	return newFXChain(time.Hour, &stubFX{name: "ECB", rates: rates})
}

func TestComputeFeedSameCurrencyNeedsNoFX(t *testing.T) {
	bm := bigMacPrice{Name: "United States", Price: 6.22, Currency: "USD"}
	quotes := []fairQuote{{Price: 0.145, Currency: "USDT", Source: "coinstore PIXAUSDT"}}

	res, err := computeFeed(bm, quotes, testFX(nil))
	if err != nil {
		t.Fatalf("computeFeed: %v", err)
	}
	q, _ := res.quoteAsset()
	if q.String() != "42.897 PIXA" {
		t.Errorf("quote = %s, want 42.897 PIXA", q)
	}
}

func TestComputeFeedConvertsIntoBigMacCurrency(t *testing.T) {
	bm := bigMacPrice{Name: "Switzerland", Price: 7.30, Currency: "CHF"}
	quotes := []fairQuote{{Price: 0.145, Currency: "USDT", Source: "coinstore PIXAUSDT"}}

	res, err := computeFeed(bm, quotes, testFX(map[string]float64{"USD/CHF": 0.8}))
	if err != nil {
		t.Fatalf("computeFeed: %v", err)
	}
	// 7.30 CHF / (0.145 USD × 0.8 CHF/USD) = 62.931 PIXA
	q, _ := res.quoteAsset()
	if q.String() != "62.931 PIXA" {
		t.Errorf("quote = %s, want 62.931 PIXA", q)
	}
	if !strings.Contains(res.summary(), "FX USD→CHF 0.8") {
		t.Errorf("summary misses the FX step: %s", res.summary())
	}
}

func TestComputeFeedIsCurrencyAgnostic(t *testing.T) {
	// The same Big Mac and the same PIXA value, expressed through two
	// different currency routes, give the same ratio.
	usdToChf := 0.8
	viaUSD, err := computeFeed(
		bigMacPrice{Price: 7.30, Currency: "CHF"},
		[]fairQuote{{Price: 0.15, Currency: "USDT"}},
		testFX(map[string]float64{"USD/CHF": usdToChf}))
	if err != nil {
		t.Fatal(err)
	}
	viaCHF, err := computeFeed(
		bigMacPrice{Price: 7.30 / usdToChf, Currency: "USD"},
		[]fairQuote{{Price: 0.15 * usdToChf, Currency: "XCHF"}},
		testFX(map[string]float64{"CHF/USD": 1 / usdToChf}))
	if err != nil {
		t.Fatal(err)
	}
	a, _ := viaUSD.quoteAsset()
	b, _ := viaCHF.quoteAsset()
	if a != b {
		t.Errorf("%s via a USD market ≠ %s via a CHF market", a, b)
	}
}

func TestComputeFeedTakesMedianOfMarkets(t *testing.T) {
	bm := bigMacPrice{Price: 6.0, Currency: "USD"}
	quotes := []fairQuote{
		{Price: 0.10, Currency: "USDT", Source: "a"},
		{Price: 0.12, Currency: "USDC", Source: "b"},
		{Price: 9.99, Currency: "USD", Source: "bad print"},
	}
	res, err := computeFeed(bm, quotes, testFX(nil))
	if err != nil {
		t.Fatal(err)
	}
	if res.PixaPerPXS != 50 {
		t.Errorf("ratio = %v, want 6.0 / median 0.12 = 50", res.PixaPerPXS)
	}
}

func TestComputeFeedFailsClosed(t *testing.T) {
	good := bigMacPrice{Price: 6.22, Currency: "USD"}
	for name, tc := range map[string]struct {
		bm     bigMacPrice
		quotes []fairQuote
		fx     fxSource
	}{
		"no quotes":     {good, nil, testFX(nil)},
		"zero price":    {good, []fairQuote{{Price: 0, Currency: "USDT"}}, testFX(nil)},
		"bad Big Mac":   {bigMacPrice{Price: 0, Currency: "USD"}, []fairQuote{{Price: 0.12, Currency: "USDT"}}, testFX(nil)},
		"no FX rate":    {bigMacPrice{Price: 3267, Currency: "VES"}, []fairQuote{{Price: 0.12, Currency: "USDT"}}, testFX(nil)},
		"FX source err": {bigMacPrice{Price: 7.3, Currency: "CHF"}, []fairQuote{{Price: 0.12, Currency: "USDT"}}, newFXChain(time.Hour, &stubFX{err: errors.New("down")})},
	} {
		if res, err := computeFeed(tc.bm, tc.quotes, tc.fx); err == nil {
			t.Errorf("%s: got %+v, want error", name, res)
		}
	}
}

func TestQuoteAssetRejectsSubPrecisionRatio(t *testing.T) {
	r := &feedResult{PixaPerPXS: 0.0004}
	if q, err := r.quoteAsset(); err == nil {
		t.Errorf("quote = %s, want error", q)
	}
}

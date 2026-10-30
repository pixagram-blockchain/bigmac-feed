package main

import (
	"fmt"
	"math"
	"strings"
)

// feedResult is one evaluation of the feed:
//
//	1 PXS = 1 Big Mac
//	PIXA per PXS = Big Mac price / PIXA fair value, both in the Big Mac's currency
//
// The PIXA fair value is the median over all markets, each converted into the
// Big Mac's currency when the market quotes PIXA in another one. The result
// is a pure ratio of a Big Mac to PIXA: no currency is privileged, whichever
// the Big Mac and the markets are priced in.
type feedResult struct {
	BigMac     bigMacPrice
	Quotes     []fairQuote // as read, in each market's currency
	Local      []float64   // Quotes converted into BigMac.Currency
	FXNotes    []string    // one line per conversion performed
	PixaLocal  float64     // median of Local
	PixaPerPXS float64
}

// computeFeed converts every quote into the Big Mac's currency and takes the
// median. It fails closed: a conversion error or a non-positive value aborts
// the round instead of publishing a guess.
func computeFeed(bm bigMacPrice, quotes []fairQuote, fx fxSource) (*feedResult, error) {
	if !(bm.Price > 0) || math.IsInf(bm.Price, 0) {
		return nil, fmt.Errorf("bad Big Mac price %v", bm.Price)
	}
	if len(quotes) == 0 {
		return nil, fmt.Errorf("no PIXA fair value from any market")
	}
	res := &feedResult{BigMac: bm, Quotes: quotes}
	seen := map[string]bool{}
	for _, q := range quotes {
		if !(q.Price > 0) || math.IsInf(q.Price, 0) {
			return nil, fmt.Errorf("%s: bad price %v", q.Source, q.Price)
		}
		rate, asOf, err := fx.Rate(q.Currency, bm.Currency)
		if err != nil {
			return nil, err
		}
		if from := canonicalCurrency(q.Currency); from != canonicalCurrency(bm.Currency) && !seen[from] {
			seen[from] = true
			res.FXNotes = append(res.FXNotes, fmt.Sprintf("%s→%s %.6g (%s)", from, canonicalCurrency(bm.Currency), rate, asOf))
		}
		res.Local = append(res.Local, q.Price*rate)
	}
	res.PixaLocal = median(res.Local)
	res.PixaPerPXS = bm.Price / res.PixaLocal
	if !(res.PixaPerPXS > 0) || math.IsInf(res.PixaPerPXS, 0) {
		return nil, fmt.Errorf("degenerate ratio %v", res.PixaPerPXS)
	}
	return res, nil
}

// quoteAsset renders the ratio as the chain asset "N.NNN PIXA".
func (r *feedResult) quoteAsset() (asset, error) {
	amount := math.Round(r.PixaPerPXS * 1000)
	if amount < 1 {
		return asset{}, fmt.Errorf("%.6f PIXA per PXS is below the chain precision of 0.001", r.PixaPerPXS)
	}
	if amount > math.MaxInt64/2 {
		return asset{}, fmt.Errorf("%.6g PIXA per PXS is out of range", r.PixaPerPXS)
	}
	return asset{Amount: int64(amount), Precision: 3, Symbol: "PIXA"}, nil
}

// summary is the one-line derivation logged every round.
func (r *feedResult) summary() string {
	var b strings.Builder
	if r.BigMac.Date != "" {
		fmt.Fprintf(&b, "Big Mac in %s = %s (as of %s)", r.BigMac.Name, r.BigMac, r.BigMac.Date)
	} else {
		fmt.Fprintf(&b, "Big Mac = %s (%s)", r.BigMac, r.BigMac.Name)
	}
	for i, q := range r.Quotes {
		fmt.Fprintf(&b, " | PIXA = %.6g %s [%s", q.Price, q.Currency, q.Source)
		if q.Detail != "" {
			fmt.Fprintf(&b, ": %s", q.Detail)
		}
		b.WriteString("]")
		if canonicalCurrency(q.Currency) != canonicalCurrency(r.BigMac.Currency) {
			fmt.Fprintf(&b, " = %.6g %s", r.Local[i], r.BigMac.Currency)
		}
	}
	for _, n := range r.FXNotes {
		fmt.Fprintf(&b, " | FX %s", n)
	}
	if len(r.Quotes) > 1 {
		fmt.Fprintf(&b, " | median PIXA = %.6g %s", r.PixaLocal, r.BigMac.Currency)
	}
	fmt.Fprintf(&b, " → 1 PXS = %.3f PIXA", r.PixaPerPXS)
	return b.String()
}

package main

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

const (
	defaultPair       = "coinstore:PIXAUSDT"
	coinstoreDepthURL = "https://api.coinstore.com/api/v1/market/depth/"
	coinstoreDepth    = 100 // order book levels requested per side
)

// --- Fair value ---
//
// The last trade is not a price: on a thin book a single 1-PIXA market order
// can print anywhere, and a feed that follows it lets anyone move the PXS
// conversion rate for a few cents. The feed therefore values PIXA at the fair
// value of the order book, the price the resting orders agree on:
//
//  1. impact bid = average price of selling impactSize PIXA into the bids,
//     impact ask = average price of buying impactSize PIXA from the asks
//     (if a side holds less, what it holds);
//  2. impact mid = (impact bid + impact ask) / 2, so dust orders at the top
//     of the book barely count;
//  3. fair value = impact mid bounded by the best bid and the best ask, since
//     a fair price outside the spread could be traded against at once.
//
// The book must be two-sided and not crossed, and its spread must be within
// maxSpread of the mid; otherwise there is no fair value and the round fails.

// level is one price level of an order book.
type level struct {
	Price float64
	Qty   float64 // in PIXA
}

// orderBook holds both sides, best price first.
type orderBook struct {
	Bids []level // highest first
	Asks []level // lowest first
}

// newOrderBook drops unusable levels and sorts each side best first, so the
// fair value does not depend on the order a venue lists its levels in.
func newOrderBook(bids, asks []level) orderBook {
	clean := func(levels []level) []level {
		var out []level
		for _, l := range levels {
			if l.Price > 0 && l.Qty > 0 && !math.IsInf(l.Price, 0) && !math.IsInf(l.Qty, 0) {
				out = append(out, l)
			}
		}
		return out
	}
	b := orderBook{Bids: clean(bids), Asks: clean(asks)}
	sort.SliceStable(b.Bids, func(i, j int) bool { return b.Bids[i].Price > b.Bids[j].Price })
	sort.SliceStable(b.Asks, func(i, j int) bool { return b.Asks[i].Price < b.Asks[j].Price })
	return b
}

// bookFair is the fair value of a book and how it was reached, for the log.
type bookFair struct {
	Fair                 float64
	BestBid, BestAsk     float64
	ImpactBid, ImpactAsk float64
	BidQty, AskQty       float64 // PIXA actually available up to impactSize
	Spread               float64 // (best ask - best bid) / mid
}

// fairValue computes the fair value of the book; see the comment at the top.
// impactSize <= 0 uses the plain mid of the best bid and ask.
func (b orderBook) fairValue(impactSize, maxSpread float64) (bookFair, error) {
	if len(b.Bids) == 0 || len(b.Asks) == 0 {
		return bookFair{}, fmt.Errorf("one-sided book (%d bids, %d asks), no fair value", len(b.Bids), len(b.Asks))
	}
	f := bookFair{BestBid: b.Bids[0].Price, BestAsk: b.Asks[0].Price}
	if f.BestBid >= f.BestAsk {
		return bookFair{}, fmt.Errorf("crossed book (bid %g >= ask %g)", f.BestBid, f.BestAsk)
	}
	f.Spread = (f.BestAsk - f.BestBid) / ((f.BestAsk + f.BestBid) / 2)
	if maxSpread > 0 && f.Spread > maxSpread {
		return bookFair{}, fmt.Errorf("spread %.1f%% (bid %g, ask %g) is wider than --max-spread %.1f%%, no fair value",
			f.Spread*100, f.BestBid, f.BestAsk, maxSpread*100)
	}
	if impactSize > 0 {
		f.ImpactBid, f.BidQty = sweep(b.Bids, impactSize)
		f.ImpactAsk, f.AskQty = sweep(b.Asks, impactSize)
	} else {
		f.ImpactBid, f.BidQty = f.BestBid, b.Bids[0].Qty
		f.ImpactAsk, f.AskQty = f.BestAsk, b.Asks[0].Qty
	}
	f.Fair = math.Min(math.Max((f.ImpactBid+f.ImpactAsk)/2, f.BestBid), f.BestAsk)
	return f, nil
}

// sweep returns the average price of filling qty against levels, best first,
// and the quantity filled (less than qty if the side runs out).
func sweep(levels []level, qty float64) (avg, filled float64) {
	var cost float64
	for _, l := range levels {
		take := math.Min(l.Qty, qty-filled)
		cost += take * l.Price
		filled += take
		if filled >= qty {
			break
		}
	}
	return cost / filled, filled
}

// --- Pairings ---

// fairQuote is the fair value of 1 PIXA on one market, in the currency that
// market quotes it in.
type fairQuote struct {
	Price    float64
	Currency string // as quoted by the venue, upper case (USDT, EUR, …)
	Source   string // e.g. "coinstore PIXAUSDT"
	Detail   string // how the fair value was reached, for the log
}

// market is one PIXA pairing on one venue.
type market interface {
	fairQuote(impactSize, maxSpread float64) (fairQuote, error)
	String() string
}

// knownQuoteCurrencies lets "coinstore:PIXAUSDT" infer its quote currency
// from the symbol suffix. Longest first, so that USDT wins over USD.
var knownQuoteCurrencies = []string{
	"FDUSD", "USDT", "USDC", "USD1", "TUSD", "PYUSD", "EURC", "EURT", "XCHF", "ZCHF",
	"USD", "EUR", "CHF", "GBP", "JPY", "BTC", "ETH", "DAI", "TRY", "BRL",
}

func inferQuote(symbol string) string {
	for _, q := range knownQuoteCurrencies {
		if strings.HasSuffix(symbol, q) && len(symbol) > len(q) {
			return q
		}
	}
	return ""
}

// parsePair turns a --pair value into a market. Accepted forms:
//
//	coinstore:PIXAUSDT          Coinstore spot order book, quote inferred (USDT)
//	coinstore:PIXAEUR:EUR       … with an explicit quote currency
func parsePair(spec string) (market, error) {
	kind, rest, ok := strings.Cut(strings.TrimSpace(spec), ":")
	if !ok {
		return nil, fmt.Errorf("pair %q: expected venue:SYMBOL, e.g. %s", spec, defaultPair)
	}
	switch strings.ToLower(kind) {
	case "coinstore":
		symbol, quote, _ := strings.Cut(rest, ":")
		symbol = strings.ToUpper(strings.TrimSpace(symbol))
		quote = strings.ToUpper(strings.TrimSpace(quote))
		if symbol == "" {
			return nil, fmt.Errorf("pair %q: missing symbol", spec)
		}
		if quote == "" {
			quote = inferQuote(symbol)
		}
		if quote == "" {
			return nil, fmt.Errorf("pair %q: cannot infer the quote currency of %s, use coinstore:%s:<QUOTE>", spec, symbol, symbol)
		}
		return coinstoreMarket{symbol: symbol, quote: quote, baseURL: coinstoreDepthURL}, nil
	}
	return nil, fmt.Errorf("pair %q: unknown venue %q (supported: coinstore)", spec, kind)
}

// --- Coinstore ---

// coinstoreMarket reads Coinstore's public order book,
// GET /api/v1/market/depth/{symbol}?depth=N (no authentication).
type coinstoreMarket struct {
	symbol  string
	quote   string
	baseURL string
}

func (c coinstoreMarket) String() string { return "coinstore " + c.symbol }

func (c coinstoreMarket) fairQuote(impactSize, maxSpread float64) (fairQuote, error) {
	book, err := fetchCoinstoreBook(c.baseURL, c.symbol)
	if err != nil {
		return fairQuote{}, fmt.Errorf("%s: %w", c, err)
	}
	f, err := book.fairValue(impactSize, maxSpread)
	if err != nil {
		return fairQuote{}, fmt.Errorf("%s: %w", c, err)
	}
	detail := fmt.Sprintf("bid %g / ask %g, %g-PIXA impact %.6g / %.6g", f.BestBid, f.BestAsk, impactSize, f.ImpactBid, f.ImpactAsk)
	if impactSize <= 0 {
		detail = fmt.Sprintf("bid %g / ask %g, mid", f.BestBid, f.BestAsk)
	}
	return fairQuote{Price: f.Fair, Currency: c.quote, Source: c.String(), Detail: detail}, nil
}

// fetchCoinstoreBook returns the order book of symbol. Each level is
// [price, quantity, side] with price and quantity as strings.
func fetchCoinstoreBook(baseURL, symbol string) (orderBook, error) {
	u := fmt.Sprintf("%s%s?depth=%d", baseURL, url.PathEscape(symbol), coinstoreDepth)
	resp, err := httpClient.Get(u)
	if err != nil {
		return orderBook{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return orderBook{}, fmt.Errorf("HTTP %s", resp.Status)
	}
	var r struct {
		Code    json.RawMessage `json:"code"` // an int, sometimes a string
		Message string          `json:"message"`
		Data    struct {
			Asks [][]json.RawMessage `json:"a"`
			Bids [][]json.RawMessage `json:"b"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return orderBook{}, err
	}
	if code := strings.Trim(string(r.Code), `"`); code != "" && code != "0" {
		return orderBook{}, fmt.Errorf("error %s: %s", code, r.Message)
	}
	bids, err := parseLevels(r.Data.Bids)
	if err != nil {
		return orderBook{}, fmt.Errorf("bids: %w", err)
	}
	asks, err := parseLevels(r.Data.Asks)
	if err != nil {
		return orderBook{}, fmt.Errorf("asks: %w", err)
	}
	return newOrderBook(bids, asks), nil
}

func parseLevels(raw [][]json.RawMessage) ([]level, error) {
	levels := make([]level, 0, len(raw))
	for _, l := range raw {
		if len(l) < 2 {
			return nil, fmt.Errorf("bad level %s", l)
		}
		price, err := jsonNumber(l[0])
		if err != nil {
			return nil, err
		}
		qty, err := jsonNumber(l[1])
		if err != nil {
			return nil, err
		}
		levels = append(levels, level{Price: price, Qty: qty})
	}
	return levels, nil
}

// jsonNumber reads a number given either as a JSON number or a string.
func jsonNumber(raw json.RawMessage) (float64, error) {
	s := strings.Trim(string(raw), `"`)
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(f) {
		return 0, fmt.Errorf("%s is not a number", raw)
	}
	return f, nil
}

// --- Aggregation ---

// fetchFairQuotes queries every market and returns what succeeded plus the
// errors of what did not.
func fetchFairQuotes(markets []market, impactSize, maxSpread float64) ([]fairQuote, []error) {
	var quotes []fairQuote
	var errs []error
	for _, m := range markets {
		q, err := m.fairQuote(impactSize, maxSpread)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		quotes = append(quotes, q)
	}
	return quotes, errs
}

func median(xs []float64) float64 {
	if len(xs) == 0 {
		return math.NaN()
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

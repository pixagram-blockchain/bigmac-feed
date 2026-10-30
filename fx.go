package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"
)

// FX is only needed when the Big Mac's currency differs from the currency
// PIXA is quoted in. Two free, keyless sources are tried in order:
//
//   - Frankfurter (https://frankfurter.dev): the ECB reference rates, ~30
//     currencies;
//   - ExchangeRate-API's open endpoint (https://www.exchangerate-api.com),
//     ~160 currencies, for the Big Mac countries the ECB does not cover
//     (ARS, EGP, VND, PKR, UAH, …).
const (
	defaultFrankfurterURL = "https://api.frankfurter.dev/v1"
	defaultERAPIURL       = "https://open.er-api.com/v6"
)

// stableAliases maps fiat-referenced stablecoins to the fiat currency they
// track: a PIXA/USDT book is read as a PIXA/USD book. For a Big Mac feed the
// stablecoin's deviation is far below the noise of the Big Mac itself.
var stableAliases = map[string]string{
	"USDT": "USD", "USDC": "USD", "USD1": "USD", "FDUSD": "USD", "TUSD": "USD",
	"DAI": "USD", "PYUSD": "USD", "BUSD": "USD",
	"EURC": "EUR", "EURT": "EUR", "EURS": "EUR", "EURI": "EUR",
	"XCHF": "CHF", "ZCHF": "CHF",
	"GBPT": "GBP",
}

// canonicalCurrency maps a venue's quote currency to the fiat it stands for.
func canonicalCurrency(c string) string {
	c = strings.ToUpper(strings.TrimSpace(c))
	if fiat, ok := stableAliases[c]; ok {
		return fiat
	}
	return c
}

// fxSource converts between currencies. Rate returns how many units of "to"
// one unit of "from" buys, and a label saying where the rate comes from.
type fxSource interface {
	Rate(from, to string) (rate float64, asOf string, err error)
}

// fxProvider is one FX API.
type fxProvider interface {
	rate(from, to string) (rate float64, date string, err error)
	String() string
}

// frankfurterFX reads GET {base}/latest?base=FROM&symbols=TO.
type frankfurterFX struct{ baseURL string }

func (f frankfurterFX) String() string { return "ECB" }

func (f frankfurterFX) rate(from, to string) (float64, string, error) {
	var r struct {
		Date  string             `json:"date"`
		Rates map[string]float64 `json:"rates"`
	}
	u := fmt.Sprintf("%s/latest?base=%s&symbols=%s", strings.TrimRight(f.baseURL, "/"), from, to)
	if err := getJSON(u, &r); err != nil {
		return 0, "", err
	}
	rate, ok := r.Rates[to]
	if !ok {
		return 0, "", fmt.Errorf("no %s rate in the response", to)
	}
	return rate, r.Date, nil
}

// erAPIFX reads GET {base}/latest/FROM, which lists every rate against FROM.
type erAPIFX struct{ baseURL string }

func (e erAPIFX) String() string { return "ExchangeRate-API" }

func (e erAPIFX) rate(from, to string) (float64, string, error) {
	var r struct {
		Result     string             `json:"result"`
		ErrorType  string             `json:"error-type"`
		UpdateUnix int64              `json:"time_last_update_unix"`
		Rates      map[string]float64 `json:"rates"`
	}
	if err := getJSON(fmt.Sprintf("%s/latest/%s", strings.TrimRight(e.baseURL, "/"), from), &r); err != nil {
		return 0, "", err
	}
	if r.Result != "success" {
		return 0, "", fmt.Errorf("%s %s", r.Result, r.ErrorType)
	}
	rate, ok := r.Rates[to]
	if !ok {
		return 0, "", fmt.Errorf("no %s rate in the response", to)
	}
	return rate, time.Unix(r.UpdateUnix, 0).UTC().Format("2006-01-02"), nil
}

func getJSON(url string, v interface{}) error {
	resp, err := httpClient.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %s", resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

type fxEntry struct {
	rate    float64
	asOf    string
	fetched time.Time
}

// fxChain asks each provider in turn and caches the last good rate per
// currency pair. If every provider fails, a cached rate is reused for up to
// maxAge: reference rates move once per business day, so a rate a few days
// old is still the best there is on a weekend.
type fxChain struct {
	providers []fxProvider
	maxAge    time.Duration
	now       func() time.Time

	mu    sync.Mutex
	cache map[string]fxEntry
}

func newFXChain(maxAge time.Duration, providers ...fxProvider) *fxChain {
	return &fxChain{providers: providers, maxAge: maxAge, now: time.Now, cache: make(map[string]fxEntry)}
}

func (c *fxChain) Rate(from, to string) (float64, string, error) {
	from, to = canonicalCurrency(from), canonicalCurrency(to)
	if from == to {
		return 1, "", nil
	}
	key := from + "/" + to

	var errs []error
	for _, p := range c.providers {
		rate, date, err := p.rate(from, to)
		if err == nil && (!(rate > 0) || math.IsInf(rate, 0)) {
			err = fmt.Errorf("bad rate %v", rate)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", p, err))
			continue
		}
		asOf := p.String() + " " + date
		c.mu.Lock()
		c.cache[key] = fxEntry{rate: rate, asOf: asOf, fetched: c.now()}
		c.mu.Unlock()
		return rate, asOf, nil
	}
	err := errors.Join(errs...)
	if len(errs) == 0 {
		err = errors.New("no FX source configured")
	}

	c.mu.Lock()
	entry, cached := c.cache[key]
	c.mu.Unlock()
	if cached && c.now().Sub(entry.fetched) <= c.maxAge {
		log.Printf("WARNING: FX %s→%s unavailable (%v); using the cached %s rate", from, to, err, entry.asOf)
		return entry.rate, entry.asOf + ", cached", nil
	}
	return 0, "", fmt.Errorf("fx %s→%s: %w", from, to, err)
}

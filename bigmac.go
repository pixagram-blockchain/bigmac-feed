package main

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/text/language"
)

const (
	defaultBigMacURL = "https://cdn.economistdatateam.com/big-mac/data/big-mac-full-index-jul-26.csv"
	geoURL           = "https://ipinfo.io/json"

	// Big Mac index country codes (iso_a3, plus EUZ for the euro area).
	usCode       = "USA"
	euroAreaCode = "EUZ"
)

// euroArea lists the euro area members by ISO 3166 alpha-3 code. The Big Mac
// index has no rows for them, only one "Euro area" row.
var euroArea = map[string]bool{
	"AUT": true, "BEL": true, "BGR": true, "CYP": true, "DEU": true,
	"ESP": true, "EST": true, "FIN": true, "FRA": true, "GRC": true,
	"HRV": true, "IRL": true, "ITA": true, "LTU": true, "LUX": true,
	"LVA": true, "MLT": true, "NLD": true, "PRT": true, "SVK": true,
	"SVN": true,
}

// --- Location ---

// indexCountry converts an ISO 3166 alpha-2 or alpha-3 country code to the
// code the Big Mac index uses for it, mapping euro area members to EUZ.
func indexCountry(code string) (string, error) {
	code = strings.ToUpper(strings.TrimSpace(code))
	if code == euroAreaCode {
		return code, nil
	}
	region, err := language.ParseRegion(code)
	if err != nil || !region.IsCountry() {
		return "", fmt.Errorf("unknown country code %q", code)
	}
	iso3 := region.ISO3()
	if euroArea[iso3] {
		return euroAreaCode, nil
	}
	return iso3, nil
}

// detectCountry geolocates this machine's public IP and returns its Big Mac
// index country code.
func detectCountry(geoURL string) (string, error) {
	resp, err := httpClient.Get(geoURL)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("geolocation: HTTP %s", resp.Status)
	}
	var r struct {
		Country string `json:"country"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return "", fmt.Errorf("geolocation: %w", err)
	}
	if r.Country == "" {
		return "", fmt.Errorf("geolocation: no country in response")
	}
	return indexCountry(r.Country)
}

// --- Big Mac price ---

// bigMacPrice is the price of one Big Mac in the local currency of the
// country it was observed in. No currency is privileged: a Swiss feeder
// prices it in CHF, a Japanese one in JPY.
type bigMacPrice struct {
	Name     string  // country name as in the index, e.g. "Euro area"
	Price    float64 // local price, in Currency
	Currency string  // ISO 4217 code, e.g. "CHF"
	Date     string  // survey date, or "" for a price given with --bigmac-price
}

func (b bigMacPrice) String() string {
	return strconv.FormatFloat(b.Price, 'f', -1, 64) + " " + b.Currency
}

// fetchBigMacPrices returns the local Big Mac prices from the latest survey
// in the index, keyed by country code. Countries dropped from the survey are
// left out.
func fetchBigMacPrices(csvURL string) (map[string]bigMacPrice, error) {
	resp, err := httpClient.Get(csvURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("big mac index: HTTP %s", resp.Status)
	}

	reader := csv.NewReader(resp.Body)
	header, err := reader.Read()
	if err != nil {
		return nil, err
	}

	col := make(map[string]int)
	for i, h := range header {
		col[strings.TrimSpace(h)] = i
	}
	for _, required := range []string{"date", "iso_a3", "name", "currency_code", "local_price"} {
		if _, ok := col[required]; !ok {
			return nil, fmt.Errorf("missing column %q", required)
		}
	}

	var latest string
	prices := make(map[string]bigMacPrice)

	for {
		row, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		date := row[col["date"]]
		if date < latest {
			continue
		}
		p, err := strconv.ParseFloat(row[col["local_price"]], 64)
		currency := strings.ToUpper(strings.TrimSpace(row[col["currency_code"]]))
		if err != nil || !(p > 0) || math.IsInf(p, 0) || !isCurrencyCode(currency) {
			continue
		}
		if date > latest {
			latest = date
			clear(prices)
		}
		prices[row[col["iso_a3"]]] = bigMacPrice{Name: row[col["name"]], Price: p, Currency: currency, Date: date}
	}

	if len(prices) == 0 {
		return nil, fmt.Errorf("no Big Mac prices found in CSV")
	}
	return prices, nil
}

// pickBigMacPrice returns the price for country. If country is not in the
// latest survey, it returns the US price when fallbackToUS is set and an
// error otherwise.
func pickBigMacPrice(prices map[string]bigMacPrice, country string, fallbackToUS bool) (bigMacPrice, error) {
	if p, ok := prices[country]; ok {
		return p, nil
	}
	if !fallbackToUS {
		return bigMacPrice{}, fmt.Errorf("%s is not in the latest Big Mac survey", country)
	}
	p, ok := prices[usCode]
	if !ok {
		return bigMacPrice{}, fmt.Errorf("US Big Mac price not found in CSV")
	}
	log.Printf("%s is not in the latest Big Mac survey, using the US price", country)
	return p, nil
}

// --- Big Mac price given by the witness ---

// priceSpecRe accepts "7.30 CHF", "CHF 7.30" and "7,30 CHF".
var priceSpecRe = regexp.MustCompile(`^\s*(?:([A-Za-z]{3})\s+)?([0-9]+(?:[.,][0-9]+)?)(?:\s*([A-Za-z]{3}))?\s*$`)

// parsePriceSpec parses a price with its currency, such as "7.30 CHF", for
// a witness that declares the Big Mac price it observes instead of using
// the index.
func parsePriceSpec(spec string) (bigMacPrice, error) {
	m := priceSpecRe.FindStringSubmatch(spec)
	if m == nil {
		return bigMacPrice{}, fmt.Errorf("cannot parse price %q (expected e.g. \"7.30 CHF\")", spec)
	}
	if m[1] != "" && m[3] != "" {
		return bigMacPrice{}, fmt.Errorf("two currencies in %q", spec)
	}
	currency := strings.ToUpper(m[1] + m[3])
	if currency == "" {
		return bigMacPrice{}, fmt.Errorf("no currency in %q (expected e.g. \"7.30 CHF\")", spec)
	}
	price, err := strconv.ParseFloat(strings.ReplaceAll(m[2], ",", "."), 64)
	if err != nil || !(price > 0) {
		return bigMacPrice{}, fmt.Errorf("bad amount in %q", spec)
	}
	return bigMacPrice{Name: "--bigmac-price", Price: price, Currency: currency}, nil
}

func isCurrencyCode(c string) bool {
	if len(c) != 3 {
		return false
	}
	for _, r := range c {
		if r < 'A' || r > 'Z' {
			return false
		}
	}
	return true
}

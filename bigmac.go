package main

import (
	"bytes"
	_ "embed"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/text/language"
)

const (
	// defaultBigMacURL is Pixa's own price file, latest.csv in this repository:
	// the pick-up price of one Big Mac per country, in the country's currency.
	// See "Price data" in the README.
	defaultBigMacURL = "https://raw.githubusercontent.com/pixagram-blockchain/bigmac-feed/main/latest.csv"
	geoURL           = "https://ipinfo.io/json"

	// euroAreaCode is accepted by --country for the euro area as a whole: the
	// median of the euro area countries that have a row in the price file.
	euroAreaCode = "EUZ"
)

// embeddedPrices is latest.csv as it was when this binary was built. The feed
// falls back to it when the published file cannot be fetched.
//
//go:embed latest.csv
var embeddedPrices []byte

// euroArea lists the euro area members by ISO 3166 alpha-2 code.
var euroArea = map[string]bool{
	"AT": true, "BE": true, "BG": true, "CY": true, "DE": true,
	"EE": true, "ES": true, "FI": true, "FR": true, "GR": true,
	"HR": true, "IE": true, "IT": true, "LT": true, "LU": true,
	"LV": true, "MT": true, "NL": true, "PT": true, "SK": true,
	"SI": true,
}

// --- Location ---

// normalizeCountry converts an ISO 3166 alpha-2 or alpha-3 country code to
// the alpha-2 code the price file is keyed by. EUZ stands for the euro area.
func normalizeCountry(code string) (string, error) {
	code = strings.ToUpper(strings.TrimSpace(code))
	if code == euroAreaCode {
		return code, nil
	}
	region, err := language.ParseRegion(code)
	if err != nil || !region.IsCountry() {
		return "", fmt.Errorf("unknown country code %q", code)
	}
	return region.String(), nil
}

// detectCountry geolocates this machine's public IP and returns its country
// code.
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
	return normalizeCountry(r.Country)
}

// --- Big Mac price ---

// bigMacPrice is the pick-up price of one Big Mac in the local currency of
// the country it was observed in: the burger on its own, as the restaurant
// sells it at the counter, the kiosk or in the McDonald's app. No currency is
// privileged: a Swiss feeder prices it in CHF, a Japanese one in JPY.
type bigMacPrice struct {
	Name     string  // where the price is from: a country code, or "--bigmac-price"
	Price    float64 // the median of the prices observed, in Currency
	Low      float64 // lowest price observed; 0 for a price given with --bigmac-price
	High     float64 // highest price observed; 0 for a price given with --bigmac-price
	Currency string  // ISO 4217 code, e.g. "CHF"
	Date     string  // date of the observation, or "" for a price given with --bigmac-price
}

func (b bigMacPrice) String() string {
	return strconv.FormatFloat(b.Price, 'f', -1, 64) + " " + b.Currency
}

// priceColumns are the columns the feed reads from the price file. The file
// also has a "sources" column, which is for people.
var priceColumns = []string{"country_code", "currency_code", "low", "high", "median", "date"}

// parseBigMacPrices reads a price file (see latest.csv) and returns its rows
// keyed by country code. A row that cannot be used (unknown country, bad
// currency, a price that is not positive, low > median or median > high, a
// date that is not YYYY-MM-DD) is left out, and so is a second row for the
// same country.
func parseBigMacPrices(r io.Reader) (map[string]bigMacPrice, error) {
	reader := csv.NewReader(r)
	reader.FieldsPerRecord = -1
	header, err := reader.Read()
	if err != nil {
		return nil, fmt.Errorf("price file: %w", err)
	}
	col := make(map[string]int)
	for i, h := range header {
		col[strings.TrimSpace(strings.TrimPrefix(h, "\ufeff"))] = i
	}
	width := 0
	for _, required := range priceColumns {
		i, ok := col[required]
		if !ok {
			return nil, fmt.Errorf("price file: missing column %q", required)
		}
		if i >= width {
			width = i + 1
		}
	}

	prices := make(map[string]bigMacPrice)
	for {
		row, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("price file: %w", err)
		}
		if len(row) < width {
			continue
		}
		country, err := normalizeCountry(row[col["country_code"]])
		if err != nil || country == euroAreaCode {
			continue
		}
		if _, seen := prices[country]; seen {
			continue
		}
		currency := strings.ToUpper(strings.TrimSpace(row[col["currency_code"]]))
		low, high, median := parsePrice(row[col["low"]]), parsePrice(row[col["high"]]), parsePrice(row[col["median"]])
		date := strings.TrimSpace(row[col["date"]])
		if _, err := time.Parse("2006-01-02", date); err != nil {
			continue
		}
		if !isCurrencyCode(currency) || !(low > 0) || low > median || median > high || math.IsInf(high, 0) {
			continue
		}
		prices[country] = bigMacPrice{Name: country, Price: median, Low: low, High: high, Currency: currency, Date: date}
	}

	if len(prices) == 0 {
		return nil, fmt.Errorf("price file: no usable row")
	}
	return prices, nil
}

// parsePrice returns NaN for anything that is not a number, so that every
// comparison with it fails.
func parsePrice(s string) float64 {
	p, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return math.NaN()
	}
	return p
}

// fetchBigMacPrices downloads and reads the price file at csvURL.
func fetchBigMacPrices(csvURL string) (map[string]bigMacPrice, error) {
	resp, err := httpClient.Get(csvURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("price file: HTTP %s", resp.Status)
	}
	return parseBigMacPrices(resp.Body)
}

// priceSource hands out the price file: the published one when it can be
// fetched, else the last copy that was, else the copy built into the binary.
// A price changes a few times a year, so an older copy is still the best
// there is while the published file is out of reach.
type priceSource struct {
	url  string // empty: use the built-in copy only
	last map[string]bigMacPrice
}

// prices returns the rows and, when they are not fresh from url, where they
// are from.
func (s *priceSource) prices() (rows map[string]bigMacPrice, origin string, err error) {
	if s.url != "" {
		rows, err := fetchBigMacPrices(s.url)
		if err == nil {
			s.last = rows
			return rows, "", nil
		}
		log.Printf("WARNING: price file %s unavailable: %v", s.url, err)
		if s.last != nil {
			return s.last, "last copy fetched", nil
		}
	}
	rows, err = parseBigMacPrices(bytes.NewReader(embeddedPrices))
	if err != nil {
		return nil, "", fmt.Errorf("built-in price file: %w", err)
	}
	return rows, "copy built into this version", nil
}

// pickBigMacPrice returns the price for country. There is no stand-in for a
// country that has no row: another country's Big Mac is another price, so
// the round fails and the witness is told how to state its own.
func pickBigMacPrice(prices map[string]bigMacPrice, country string) (bigMacPrice, error) {
	if p, ok := prices[country]; ok {
		return p, nil
	}
	if country == euroAreaCode {
		return euroAreaPrice(prices)
	}
	return bigMacPrice{}, fmt.Errorf("no pick-up price for %s in the price file yet: state the price you pay with --bigmac-price (e.g. --bigmac-price=\"7.30 CHF\") and report it so it can be added, see the README", country)
}

// euroAreaPrice is the median of the euro area countries that have a row,
// for a witness that asks for the euro area as a whole with --country=EUZ.
func euroAreaPrice(prices map[string]bigMacPrice) (bigMacPrice, error) {
	var codes []string
	for code, p := range prices {
		if euroArea[code] && p.Currency == "EUR" {
			codes = append(codes, code)
		}
	}
	if len(codes) == 0 {
		return bigMacPrice{}, fmt.Errorf("no euro area country in the price file")
	}
	sort.Strings(codes)
	medians := make([]float64, len(codes))
	out := bigMacPrice{Currency: "EUR", Name: "the euro area (median of " + strings.Join(codes, ", ") + ")"}
	for i, code := range codes {
		p := prices[code]
		medians[i] = p.Price
		if out.Low == 0 || p.Low < out.Low {
			out.Low = p.Low
		}
		if p.High > out.High {
			out.High = p.High
		}
		if out.Date == "" || p.Date < out.Date {
			out.Date = p.Date // the oldest observation, so the log does not overstate freshness
		}
	}
	out.Price = median(medians)
	return out, nil
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

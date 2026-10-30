package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"
)

// pairList is a repeatable flag ("--pair a --pair b") that also accepts a
// comma-separated list.
type pairList []string

func (p *pairList) String() string { return strings.Join(*p, ",") }
func (p *pairList) Set(v string) error {
	for _, part := range strings.Split(v, ",") {
		if part = strings.TrimSpace(part); part != "" {
			*p = append(*p, part)
		}
	}
	return nil
}

func main() {
	witness := flag.String("witness", "initminer", "Witness account name(s), comma-separated")
	wif := flag.String("wif", "", "Witness active key(s) (WIF), comma-separated in the same order as --witness. Also reads WITNESS_WIF env.")
	rpc := flag.String("rpc", "https://api.pixagram.com", "RPC endpoint")
	chainID := flag.String("chain-id", "", "Chain ID (hex). If empty, fetched from --rpc via database_api.get_config.")
	interval := flag.Duration("interval", 1*time.Hour, "Feed publish interval")
	once := flag.Bool("once", false, "Publish once and exit")
	dryRun := flag.Bool("dry-run", false, "Compute and log the feed without broadcasting; no WIF needed")

	// Big Mac
	countryFlag := flag.String("country", "", "Country whose Big Mac price to use, as an ISO 3166 code (e.g. CH, CHE) or EUZ for the euro area. If empty, detected from this machine's public IP, falling back to the US.")
	bigMacInline := flag.String("bigmac-price", "", "Big Mac price observed by this witness, with its currency (e.g. \"7.30 CHF\"). Overrides --country and the index.")
	bigMacURL := flag.String("bigmac-url", defaultBigMacURL, "CSV of The Economist's Big Mac index")

	// PIXA
	var pairs pairList
	flag.Var(&pairs, "pair", "PIXA market(s) to value PIXA on, repeatable or comma-separated: coinstore:SYMBOL or coinstore:SYMBOL:QUOTE (default "+defaultPair+"). Several markets → median.")
	impactSize := flag.Float64("impact-size", 1000, "Order size in PIXA used to read the fair value from the order book; 0 = plain mid of the best bid and ask")
	maxSpread := flag.Float64("max-spread", 0.25, "Widest bid/ask spread, relative to the mid, at which a book still has a fair value (0.25 = 25%); 0 disables")
	tokenPrice := flag.Float64("token-price", 0, "Placeholder PIXA price, used only when no market gives a fair value (e.g. before a listing); 0 = off, the round fails instead")
	tokenCurrency := flag.String("token-currency", "USD", "Currency of --token-price")

	// FX
	fxURL := flag.String("fx-url", defaultFrankfurterURL, "Frankfurter (ECB rates) API base URL; empty disables")
	fxFallbackURL := flag.String("fx-fallback-url", defaultERAPIURL, "ExchangeRate-API open endpoint base URL, for currencies the ECB does not cover; empty disables")
	fxMaxAge := flag.Duration("fx-max-age", 96*time.Hour, "How long a cached FX rate may be reused while every FX source is down")
	flag.Parse()

	// --- witnesses ---
	if *wif == "" {
		*wif = os.Getenv("WITNESS_WIF")
	}
	var witnesses []witnessKey
	switch {
	case *wif != "":
		var err error
		if witnesses, err = parseWitnesses(*witness, *wif); err != nil {
			log.Fatal(err)
		}
	case *dryRun:
		for _, name := range splitList(*witness) {
			witnesses = append(witnesses, witnessKey{name: name})
		}
	default:
		log.Fatal("provide --wif or set WITNESS_WIF env (or use --dry-run)")
	}

	if *chainID == "" && !*dryRun {
		cfg, err := rpcCall(*rpc, "database_api.get_config", map[string]interface{}{})
		if err != nil {
			log.Fatalf("fetch chain_id: %v", err)
		}
		var parsed struct {
			HiveChainID string `json:"HIVE_CHAIN_ID"`
		}
		if err := json.Unmarshal(cfg, &parsed); err != nil || parsed.HiveChainID == "" {
			log.Fatalf("fetch chain_id: could not parse HIVE_CHAIN_ID from get_config")
		}
		*chainID = parsed.HiveChainID
		log.Printf("Auto-detected chain_id: %s", *chainID)
	}

	// --- Big Mac source: --bigmac-price, else the index for the country ---
	var bigMac func() (bigMacPrice, error)
	if *bigMacInline != "" {
		p, err := parsePriceSpec(*bigMacInline)
		if err != nil {
			log.Fatalf("--bigmac-price: %v", err)
		}
		log.Printf("Big Mac price: %s (from --bigmac-price)", p)
		bigMac = func() (bigMacPrice, error) { return p, nil }
	} else {
		// An explicit --country must be in the survey; a detected one falls back to the US.
		country, fallbackToUS := *countryFlag, false
		var err error
		if country != "" {
			if country, err = indexCountry(country); err != nil {
				log.Fatalf("--country: %v", err)
			}
			log.Printf("Big Mac country: %s (from --country)", country)
		} else if country, err = detectCountry(geoURL); err != nil {
			country = usCode
			log.Printf("Big Mac country: %s (IP geolocation failed: %v)", country, err)
		} else {
			fallbackToUS = true
			log.Printf("Big Mac country: %s (detected from public IP)", country)
		}
		bigMac = func() (bigMacPrice, error) {
			prices, err := fetchBigMacPrices(*bigMacURL)
			if err != nil {
				return bigMacPrice{}, fmt.Errorf("fetch Big Mac price: %w", err)
			}
			return pickBigMacPrice(prices, country, fallbackToUS)
		}
	}

	// --- PIXA markets ---
	if len(pairs) == 0 {
		pairs.Set(defaultPair)
	}
	var markets []market
	for _, spec := range pairs {
		m, err := parsePair(spec)
		if err != nil {
			log.Fatalf("--pair: %v", err)
		}
		markets = append(markets, m)
	}
	if *tokenPrice < 0 {
		log.Fatal("--token-price must not be negative")
	}
	*tokenCurrency = strings.ToUpper(strings.TrimSpace(*tokenCurrency))

	// --- FX ---
	var providers []fxProvider
	if *fxURL != "" {
		providers = append(providers, frankfurterFX{baseURL: *fxURL})
	}
	if *fxFallbackURL != "" {
		providers = append(providers, erAPIFX{baseURL: *fxFallbackURL})
	}
	fx := newFXChain(*fxMaxAge, providers...)

	names := make([]string, len(markets))
	for i, m := range markets {
		names[i] = m.String()
	}
	log.Printf("Big Mac Feed — witnesses=%s rpc=%s markets=[%s] impact-size=%g PIXA max-spread=%g%% interval=%s dry-run=%v",
		*witness, *rpc, strings.Join(names, ", "), *impactSize, *maxSpread*100, *interval, *dryRun)

	publish := func() error {
		bm, err := bigMac()
		if err != nil {
			return err
		}
		quotes, errs := fetchFairQuotes(markets, *impactSize, *maxSpread)
		for _, e := range errs {
			log.Printf("WARNING: no fair value: %v", e)
		}
		if len(quotes) == 0 {
			if *tokenPrice == 0 {
				return fmt.Errorf("no PIXA fair value from any of the %d market(s); nothing published, the chain keeps the previous feed", len(markets))
			}
			log.Printf("WARNING: no market gives a fair value, using the --token-price placeholder %g %s", *tokenPrice, *tokenCurrency)
			quotes = []fairQuote{{Price: *tokenPrice, Currency: *tokenCurrency, Source: "--token-price placeholder"}}
		}
		res, err := computeFeed(bm, quotes, fx)
		if err != nil {
			return err
		}
		quote, err := res.quoteAsset()
		if err != nil {
			return err
		}
		log.Print(res.summary())

		base := asset{Amount: 1000, Precision: 3, Symbol: "PXS"} // 1 PXS = 1 Big Mac
		if *dryRun {
			log.Printf("Dry run: would publish 1.000 PXS = %s for %s", quote, *witness)
			return nil
		}
		return publishAll(*rpc, *chainID, witnesses, base, quote)
	}

	if err := publish(); err != nil {
		log.Printf("ERROR: %v", err)
		if *once {
			os.Exit(1)
		}
	}

	if *once {
		return
	}

	ticker := time.NewTicker(*interval)
	defer ticker.Stop()
	for range ticker.C {
		if err := publish(); err != nil {
			log.Printf("ERROR: %v", err)
		}
	}
}

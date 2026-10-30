package main

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

// stubFX is an fxProvider with a fixed answer.
type stubFX struct {
	name  string
	rates map[string]float64 // "USD/CHF" → rate
	err   error
	calls int
}

func (s *stubFX) String() string { return s.name }
func (s *stubFX) rate(from, to string) (float64, string, error) {
	s.calls++
	if s.err != nil {
		return 0, "", s.err
	}
	r, ok := s.rates[from+"/"+to]
	if !ok {
		return 0, "", errors.New("not found")
	}
	return r, "2026-10-29", nil
}

func TestFXChainSameCurrencyNeedsNoProvider(t *testing.T) {
	p := &stubFX{name: "p", err: errors.New("down")}
	rate, _, err := newFXChain(time.Hour, p).Rate("USDT", "USD")
	if err != nil || rate != 1 || p.calls != 0 {
		t.Fatalf("USDT→USD = %v, %v after %d calls; want 1 with no call", rate, err, p.calls)
	}
}

func TestFXChainFallsBackToNextProvider(t *testing.T) {
	ecb := &stubFX{name: "ECB", rates: map[string]float64{"USD/CHF": 0.83}}
	wide := &stubFX{name: "wide", rates: map[string]float64{"USD/ARS": 1540}}
	fx := newFXChain(time.Hour, ecb, wide)

	if rate, asOf, err := fx.Rate("USDT", "CHF"); err != nil || rate != 0.83 || !strings.HasPrefix(asOf, "ECB") {
		t.Errorf("USDT→CHF = %v (%s), %v; want 0.83 from ECB", rate, asOf, err)
	}
	if rate, asOf, err := fx.Rate("USD", "ARS"); err != nil || rate != 1540 || !strings.HasPrefix(asOf, "wide") {
		t.Errorf("USD→ARS = %v (%s), %v; want 1540 from the fallback", rate, asOf, err)
	}
}

func TestFXChainReusesCachedRateWithinMaxAge(t *testing.T) {
	p := &stubFX{name: "ECB", rates: map[string]float64{"USD/CHF": 0.83}}
	fx := newFXChain(96*time.Hour, p)
	now := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
	fx.now = func() time.Time { return now }

	if _, _, err := fx.Rate("USD", "CHF"); err != nil {
		t.Fatal(err)
	}
	p.err = errors.New("down")

	now = now.Add(72 * time.Hour)
	if rate, asOf, err := fx.Rate("USD", "CHF"); err != nil || rate != 0.83 || !strings.Contains(asOf, "cached") {
		t.Errorf("after 72h = %v (%s), %v; want the cached 0.83", rate, asOf, err)
	}
	now = now.Add(48 * time.Hour)
	if rate, _, err := fx.Rate("USD", "CHF"); err == nil {
		t.Errorf("after 120h = %v, want error (cache older than max age)", rate)
	}
}

func TestFXChainFailsWithoutAnyRate(t *testing.T) {
	_, _, err := newFXChain(time.Hour, &stubFX{name: "ECB"}).Rate("USD", "VES")
	if err == nil || !strings.Contains(err.Error(), "USD→VES") {
		t.Fatalf("expected error naming USD→VES, got %v", err)
	}
}

func TestFrankfurterFXParsesResponse(t *testing.T) {
	var query string
	srv := handlerServer(t, func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.RequestURI()
		w.Write([]byte(`{"amount":1.0,"base":"USD","date":"2026-10-29","rates":{"CHF":0.82857}}`))
	})

	rate, date, err := frankfurterFX{srv.URL}.rate("USD", "CHF")
	if err != nil || rate != 0.82857 || date != "2026-10-29" {
		t.Fatalf("rate = %v %s, %v", rate, date, err)
	}
	if query != "/latest?base=USD&symbols=CHF" {
		t.Errorf("requested %q", query)
	}
}

func TestFrankfurterFXReportsUnsupportedCurrency(t *testing.T) {
	srv := staticServer(t, http.StatusNotFound, `{"message":"not found"}`)

	if _, _, err := (frankfurterFX{srv.URL}).rate("USD", "ARS"); err == nil {
		t.Fatal("expected error for HTTP 404")
	}
}

func TestERAPIFXParsesResponse(t *testing.T) {
	srv := staticServer(t, http.StatusOK, `{"result":"success","time_last_update_unix":1793318551,"base_code":"USD","rates":{"USD":1,"ARS":1539.59}}`)

	rate, date, err := erAPIFX{srv.URL}.rate("USD", "ARS")
	if err != nil || rate != 1539.59 || date != "2026-10-30" {
		t.Fatalf("rate = %v %s, %v", rate, date, err)
	}
}

func TestERAPIFXReportsError(t *testing.T) {
	srv := staticServer(t, http.StatusOK, `{"result":"error","error-type":"unsupported-code"}`)

	if _, _, err := (erAPIFX{srv.URL}).rate("XYZ", "USD"); err == nil || !strings.Contains(err.Error(), "unsupported-code") {
		t.Fatalf("expected unsupported-code error, got %v", err)
	}
}

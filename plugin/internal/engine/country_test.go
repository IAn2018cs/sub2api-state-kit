package engine

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCountryLookupUsesObservedIPAndCaches(t *testing.T) {
	var hits atomic.Int32
	geo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path != "/203.0.113.8" || r.Header.Get("Authorization") != "" || r.Header.Get("Proxy-Authorization") != "" || r.Header.Get(StateHeader) != "" {
			t.Error("lookup target or credentials incorrect")
		}
		io.WriteString(w, `{"ip":"203.0.113.8","country":"JP"}`)
	}))
	defer geo.Close()
	e := newEngine(testHost(), "", time.Hour)
	defer e.Close()
	e.countryURL = geo.URL
	for i := 0; i < 2; i++ {
		if e.countryForIP(context.Background(), "203.0.113.8") != "JP" {
			t.Fatal("country not resolved")
		}
	}
	if hits.Load() != 1 {
		t.Fatal("country cache not used")
	}
	if e.countryForIP(context.Background(), "127.0.0.1") != "" || e.countryForIP(context.Background(), "garbage") != "" {
		t.Fatal("invalid IP accepted")
	}
}
func TestCountryFailureIsUnknownAndNonFatal(t *testing.T) {
	for _, body := range []string{`{"ip":"203.0.113.99","country":"JP"}`, `{"ip":"203.0.113.8","country":"<script>"}`, `unavailable`} {
		geo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, body) }))
		e := newEngine(testHost(), "", time.Hour)
		e.countryURL = geo.URL
		if e.countryForIP(context.Background(), "203.0.113.8") != "" {
			t.Fatal("untrusted country accepted")
		}
		e.Close()
		geo.Close()
	}
}
func TestEachHarvestAttemptRecordsActualCountry(t *testing.T) {
	var ipChecks atomic.Int32
	ip := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprintf(w, `{"ip":"203.0.113.%d"}`, ipChecks.Add(1)) }))
	defer ip.Close()
	geo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		code := "JP"
		if strings.HasSuffix(r.URL.Path, ".2") {
			code = "DE"
		}
		fmt.Fprintf(w, `{"ip":%q,"country":%q}`, strings.TrimPrefix(r.URL.Path, "/"), code)
	}))
	defer geo.Close()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(StateHeader, testState(292))
		completed(w, "gpt-6-astra")
	}))
	defer up.Close()
	host := testHost(7)
	e := newEngine(host, up.URL, time.Hour)
	defer e.Close()
	e.exitIPURL = ip.URL
	e.countryURL = geo.URL
	k := keyFor(7, "gpt-6-astra")
	e.mu.Lock()
	e.generation = 1
	e.jobs[k] = 1
	e.records[k] = &jobRecord{Trigger: "manual"}
	e.mu.Unlock()
	identity, err := resolveIdentity(context.Background(), host, 7)
	if err != nil {
		t.Fatal(err)
	}
	for n := 1; n <= 2; n++ {
		if _, _, err := e.observedProbeChecked(context.Background(), DefaultConfig(), identity, "gpt-6-astra", "", "", k, 1, n, "harvest", true); err != nil {
			t.Fatal(err)
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	matched := map[int]string{}
	for _, event := range e.events {
		if event.Phase == "harvest" && event.Result == "model_matched" {
			matched[event.Attempt] = event.CountryCode
			if event.Trigger != "manual" {
				t.Fatal("missing origin")
			}
		}
	}
	if matched[1] != "JP" || matched[2] != "DE" || e.records[k].CountryCode != "DE" {
		t.Fatalf("wrong per-attempt countries: %+v", matched)
	}
}

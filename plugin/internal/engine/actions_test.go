package engine

import (
	"context"
	"encoding/json"
	"fmt"
	pluginv1 "github.com/wangyunjeff/sub2api-state-kit/plugin/internal/pluginapi/v1"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func action(t *testing.T, e *Engine, a manualAction) *pluginv1.RunActionResponse {
	t.Helper()
	r, err := e.RunAction(context.Background(), &pluginv1.RunActionRequest{ActionJson: []byte(jsonText(a))})
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func waitManual(t *testing.T, e *Engine) manualResult {
	t.Helper()
	for until := time.Now().Add(3 * time.Second); time.Now().Before(until); time.Sleep(time.Millisecond) {
		e.mu.Lock()
		r := e.manual
		var copy manualResult
		if r != nil {
			copy = *r
		}
		e.mu.Unlock()
		if r != nil && !copy.Running {
			return copy
		}
	}
	t.Fatal("manual test timeout")
	return manualResult{}
}
func TestFailOpenStillUsesBusinessRouteAndInjectsAvailableTicket(t *testing.T) {
	if !DefaultConfig().AllowWithoutTicket {
		t.Fatal("default must allow")
	}
	var got []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Header.Get(StateHeader))
		io.WriteString(w, "ok")
	}))
	defer up.Close()
	e := forwardingEngine(t, true)
	e.config.AllowWithoutTicket = true
	body := []byte(`{"model":"gpt-test"}`)
	for i := 0; i < 2; i++ {
		if i == 1 {
			addForwardTicket(e, "")
		}
		s := fixedStream(mockStart(up.URL, true, int64(len(body))), body)
		if err := e.Forward(s); err != nil {
			t.Fatal(err)
		}
		if s.frames()[0].GetStart().StatusCode != 200 {
			t.Fatal("blocked request")
		}
	}
	if len(got) != 2 || got[0] != "" || len(got[1]) != 292 {
		t.Fatal("missing or wrong STATE injection")
	}
}
func TestManualPromptIPAndHTMLResult(t *testing.T) {
	var hits atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			if r.Header.Get("Authorization") != "" {
				t.Error("token leaked to IP endpoint")
			}
			io.WriteString(w, `{"ip":"203.0.113.7"}`)
			return
		}
		hits.Add(1)
		var p map[string]any
		json.NewDecoder(r.Body).Decode(&p)
		if !strings.Contains(jsonText(p), "test prompt") {
			t.Error("prompt not sent")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: "+`{"type":"response.output_text.delta","delta":"<html>test</html>"}`+"\n\ndata: "+`{"type":"response.completed","response":{"status":"completed","model":"gpt-test"}}`+"\n\n")
	}))
	defer up.Close()
	e := newEngine(testHost(7), up.URL, time.Hour)
	defer e.Close()
	e.exitIPURL = up.URL
	e.refreshDirectory()
	a := manualAction{Kind: "test", RequestID: "one", AccountID: 7, Model: "gpt-test", Prompt: "test prompt", ExpectedIP: "203.0.113.7"}
	if !action(t, e, a).Accepted {
		t.Fatal("rejected")
	}
	r := waitManual(t, e)
	if !r.Complete || !r.Matches || r.Injected || r.Text != "<html>test</html>" || r.IPMatches == nil || !*r.IPMatches {
		t.Fatalf("bad result %+v", r)
	}
	action(t, e, a)
	if hits.Load() != 1 {
		t.Fatal("duplicate action replayed")
	}
	if action(t, e, manualAction{Kind: "test", RequestID: "bad", AccountID: 999, Model: "gpt-test", Prompt: "test prompt"}).Accepted {
		t.Fatal("unknown account accepted")
	}
	a.Kind = "ip"
	a.RequestID = "ip"
	a.Route = "direct"
	action(t, e, a)
	r = waitManual(t, e)
	if r.ExitIP != "203.0.113.7" || hits.Load() != 1 {
		t.Fatal("IP test called model")
	}
}
func TestManualCancelAndNoConcurrentTest(t *testing.T) {
	entered := make(chan struct{}, 1)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { entered <- struct{}{}; <-r.Context().Done() }))
	defer up.Close()
	e := newEngine(testHost(7), up.URL, time.Hour)
	defer e.Close()
	e.exitIPURL = up.URL
	e.refreshDirectory()
	a := manualAction{Kind: "ip", RequestID: "one", AccountID: 7, Route: "business"}
	action(t, e, a)
	<-entered
	a.RequestID = "two"
	if action(t, e, a).Accepted {
		t.Fatal("concurrent test accepted")
	}
	action(t, e, manualAction{Kind: "cancel_test", RequestID: "cancel"})
	r := waitManual(t, e)
	if r.Error != "测试已取消" {
		t.Fatal("cancel not distinguished from timeout")
	}
	if action(t, e, manualAction{Kind: "cancel_test", RequestID: "cancel-idle"}).Accepted {
		t.Fatal("idle cancellation reported as active")
	}
}

func TestManualHTTP200UpstreamFailure(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"overload", `{"type":"error","error":{"code":"server_is_overloaded","message":"private upstream details"}}`, "server_is_overloaded"},
		{"failed", `{"type":"response.failed","response":{"status":"failed","error":{"code":"rate_limit_exceeded"}}}`, "rate_limit_exceeded"},
		{"incomplete", `{"type":"response.incomplete","response":{"status":"incomplete"}}`, "未完成响应"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "GET" {
					io.WriteString(w, `{"ip":"203.0.113.7"}`)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprintf(w, "data: %s\n\n", tc.body)
			}))
			defer up.Close()
			e := newEngine(testHost(7), up.URL, time.Hour)
			defer e.Close()
			e.exitIPURL = up.URL
			e.refreshDirectory()
			if !action(t, e, manualAction{Kind: "test", RequestID: tc.name, AccountID: 7, Model: "gpt-test", Prompt: "test"}).Accepted {
				t.Fatal("rejected")
			}
			r := waitManual(t, e)
			if r.HTTPStatus != 200 || r.Complete || r.Matches || r.ActualModel != "" || !strings.Contains(r.Error, tc.want) || strings.Contains(r.Error, "private") {
				t.Fatalf("wrong failure result: %+v", r)
			}
		})
	}
}
func TestManualRefreshBypassesCooldownAndRestoration(t *testing.T) {
	var hits atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			io.WriteString(w, `{"ip":"203.0.113.8"}`)
			return
		}
		hits.Add(1)
		w.Header().Set(StateHeader, testState(292))
		completed(w, "gpt-6-astra")
	}))
	defer up.Close()
	h := testHost(7)
	e := testEngine(t, h, up.URL)
	e.exitIPURL = up.URL
	c := testConfig("http://unused.test", 7)
	// Use a functioning dynamic proxy; force should create new upstream work, not only restore KV.
	var proxyHits atomic.Int32
	c.DynamicProxyURL = testHTTPProxy(t, &proxyHits, false)
	apply(t, e, c)
	waitFor(t, e, "ready")
	before := hits.Load()
	e.mu.Lock()
	e.records[keyFor(7, "gpt-6-astra")] = &jobRecord{CooldownUntil: time.Now().Add(time.Hour)}
	e.mu.Unlock()
	if !action(t, e, manualAction{Kind: "refresh", RequestID: "refresh", AccountID: 7}).Accepted {
		t.Fatal("refresh rejected")
	}
	for until := time.Now().Add(2 * time.Second); time.Now().Before(until) && hits.Load() < before+2; time.Sleep(time.Millisecond) {
	}
	if hits.Load() < before+2 {
		t.Fatal("manual refresh did not bypass cooldown")
	}
}

func TestProxyDraftConnectivityWithoutAccountOrModel(t *testing.T) {
	var hits atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.Header.Get("Authorization") != "" || r.Header.Get(StateHeader) != "" {
			t.Error("proxy connectivity must not send model or account credentials")
		}
		hits.Add(1)
		io.WriteString(w, `{"ip":"203.0.113.5"}`)
	}))
	defer up.Close()
	var proxyHits atomic.Int32
	proxy := testHTTPProxy(t, &proxyHits, false)
	host := testHost()
	e := newEngine(host, up.URL, time.Hour)
	defer e.Close()
	e.exitIPURL = up.URL
	before := jsonText(e.config)
	a := manualAction{Kind: "proxy_test", RequestID: "draft", Proxy: &proxyTestConfig{DynamicProxyURL: proxy, Mode: "direct"}}
	if !action(t, e, a).Accepted {
		t.Fatal("must accept without enabled account")
	}
	result := waitManual(t, e)
	if !result.Complete || result.ExitIP != "203.0.113.5" || hits.Load() != 1 || proxyHits.Load() != 1 {
		t.Fatalf("connectivity failed: %+v", result)
	}
	host.mu.Lock()
	resolves, sets := host.resolves, host.sets
	host.mu.Unlock()
	if resolves != 0 || sets != 0 || jsonText(e.config) != before {
		t.Fatal("connectivity must not resolve OAuth, persist STATE or modify config")
	}
	a.RequestID = "invalid"
	a.Proxy.DynamicProxyURL = "file:///nope"
	if action(t, e, a).Accepted {
		t.Fatal("invalid draft accepted")
	}
}

func TestManualRefreshStopsBeforeModelOnConnectivityFailure(t *testing.T) {
	for _, route := range []string{"business", "dynamic"} {
		t.Run(route, func(t *testing.T) {
			var modelHits atomic.Int32
			ip := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" {
					modelHits.Add(1)
					t.Error("model requested before connectivity passed")
				}
				if r.Header.Get("Authorization") != "" {
					t.Error("OAuth sent in connectivity check")
				}
				if route == "business" {
					w.WriteHeader(503)
					return
				}
				io.WriteString(w, `{"ip":"203.0.113.8"}`)
			}))
			defer ip.Close()
			host := testHost(7)
			e := newEngine(host, ip.URL, time.Hour)
			defer e.Close()
			e.exitIPURL = ip.URL
			c := testConfig("http://127.0.0.1:1", 7)
			k := keyFor(7, "gpt-6-astra")
			e.mu.Lock()
			e.config = c
			e.generation = 1
			e.directory[7] = true
			e.jobs[k] = 1
			e.wg.Add(1)
			e.mu.Unlock()
			e.collect(e.ctx, host, c, c.Accounts[0], "gpt-6-astra", k, 1, true)
			e.mu.Lock()
			record := *e.records[k]
			e.mu.Unlock()
			if record.LastError != route+"_connectivity_failed" || modelHits.Load() != 0 {
				t.Fatalf("unexpected result %s model hits %d", record.LastError, modelHits.Load())
			}
			if !record.CooldownUntil.After(time.Now()) {
				t.Fatal("failure must stop the manual round")
			}
		})
	}
}

func TestSingleRefreshIsolationAndExplicitAllRefresh(t *testing.T) {
	release := make(chan struct{})
	ip := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
			io.WriteString(w, `{"ip":"203.0.113.8"}`)
		case <-r.Context().Done():
		}
	}))
	defer ip.Close()
	host := testHost(7, 8, 9)
	e := newEngine(host, ip.URL, time.Hour)
	defer e.Close()
	defer close(release)
	e.exitIPURL = ip.URL
	c := testConfig("http://127.0.0.1:1", 7)
	c.Accounts = append(c.Accounts, AccountConfig{AccountID: 8, Enabled: true, Plan: "pro", Models: []string{"gpt-6-astra"}}, AccountConfig{AccountID: 9, Enabled: false, Plan: "pro", Models: []string{"gpt-6-astra"}})
	e.mu.Lock()
	e.warmup = time.Hour
	e.mu.Unlock()
	apply(t, e, c)
	e.refreshDirectory()
	until := time.Now().Add(2 * time.Hour)
	e.mu.Lock()
	originalAfter := e.activeAfter
	e.records[keyFor(8, "gpt-6-astra")] = &jobRecord{CooldownUntil: until, Attempts: 3, LastError: "keep-me"}
	e.mu.Unlock()
	if !action(t, e, manualAction{Kind: "refresh", RequestID: "single", AccountID: 7}).Accepted {
		t.Fatal("single rejected")
	}
	e.schedule()
	e.mu.Lock()
	if len(e.jobs) != 1 || e.jobs[keyFor(7, "gpt-6-astra")] == 0 || !e.activeAfter.Equal(originalAfter) || !e.records[keyFor(8, "gpt-6-astra")].CooldownUntil.Equal(until) || e.records[keyFor(8, "gpt-6-astra")].LastError != "keep-me" {
		t.Fatal("single refresh changed other accounts or global schedule")
	}
	e.mu.Unlock()
	if !action(t, e, manualAction{Kind: "refresh_all", RequestID: "all"}).Accepted {
		t.Fatal("bulk rejected")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.jobs) != 2 || e.jobs[keyFor(9, "gpt-6-astra")] != 0 {
		t.Fatal("bulk must start enabled accounts only, with no duplicates")
	}
	if e.records[keyFor(7, "gpt-6-astra")].Trigger != "manual" || e.records[keyFor(8, "gpt-6-astra")].Trigger != "manual_all" {
		t.Fatal("action origins missing")
	}
}

func TestAutomaticOffSchedulesOnlyExplicitAccount(t *testing.T) {
	h := testHost(7, 8)
	e := newEngine(h, "http://127.0.0.1:1", time.Hour)
	defer e.Close()
	e.exitIPURL = "http://127.0.0.1:1"
	c := testConfig("http://127.0.0.1:1", 7)
	c.AutoHarvest = false
	c.Accounts = append(c.Accounts, AccountConfig{AccountID: 8, Enabled: true, Plan: "pro", Models: []string{"gpt-6-astra"}})
	apply(t, e, c)
	e.refreshDirectory()
	e.mu.Lock()
	e.activeAfter = time.Now().Add(-time.Hour)
	e.mu.Unlock()
	e.schedule()
	e.mu.Lock()
	if len(e.jobs) != 0 {
		t.Fatal("automatic scheduler ran while off")
	}
	e.mu.Unlock()
	if !action(t, e, manualAction{Kind: "refresh", AccountID: 7, RequestID: "one-only"}).Accepted {
		t.Fatal("manual start disabled")
	}
	time.Sleep(20 * time.Millisecond)
	e.schedule()
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.records[keyFor(7, "gpt-6-astra")] == nil || e.records[keyFor(8, "gpt-6-astra")] != nil {
		t.Fatal("unselected account ran")
	}
}

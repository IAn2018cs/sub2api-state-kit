package engine

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pluginv1 "github.com/wangyunjeff/sub2api-state-kit/plugin/internal/pluginapi/v1"
)

func connectivityFixture(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		io.WriteString(w, `{"ip":"203.0.113.8"}`)
		return
	}
	w.Header().Set(StateHeader, testState(292))
	completed(w, "gpt-6-astra")
}

func TestProxyTestSessionSurvivesSaveAndIsUsedForFirstManualAttempt(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(connectivityFixture))
	defer up.Close()
	var mu sync.Mutex
	var sessions []string
	dynamic := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		sessions = append(sessions, r.Header.Get("Proxy-Authorization"))
		mu.Unlock()
		if r.Method == http.MethodGet && (r.Header.Get("Authorization") != "" || r.Header.Get(StateHeader) != "") {
			t.Error("account credentials leaked to IP service")
		}
		connectivityFixture(w, r)
	}))
	defer dynamic.Close()
	h := testHost(7)
	e := newEngine(h, up.URL, time.Hour)
	defer e.Close()
	e.exitIPURL = up.URL
	pool := strings.Replace(dynamic.URL, "http://", "http://user-{sid}:private-test-password@", 1)
	a := manualAction{Kind: "proxy_test", RequestID: "draft", Proxy: &proxyTestConfig{DynamicProxyURL: pool, Mode: "direct"}}
	if !action(t, e, a).Accepted {
		t.Fatal("test rejected")
	}
	result := waitManual(t, e)
	if !result.Complete || result.ProxySessionExpiresAt == "" {
		t.Fatal("successful sample session was not retained")
	}
	// This is the UI's subsequent config save. It must not drop the sample.
	c := testConfig(pool, 7)
	c.AutoHarvest = false
	apply(t, e, c)
	e.mu.Lock()
	e.directory[7] = true
	e.mu.Unlock()
	if !action(t, e, manualAction{Kind: "refresh", RequestID: "collect", AccountID: 7}).Accepted {
		t.Fatal("refresh rejected")
	}
	waitFor(t, e, "ready")
	mu.Lock()
	defer mu.Unlock()
	if len(sessions) != 3 || sessions[0] == "" || sessions[0] != sessions[1] || sessions[1] != sessions[2] {
		t.Fatalf("IP test and model probe did not keep the same authenticated session; requests=%d", len(sessions))
	}
	e.mu.Lock()
	reused := false
	for _, event := range e.events {
		if event.Phase == "harvest" && event.ReusedProxySession {
			reused = true
		}
	}
	e.mu.Unlock()
	if !reused {
		t.Fatal("reuse not visible in activity")
	}
	status, _ := e.Health(context.Background(), &pluginv1.HealthRequest{})
	if strings.Contains(status.StatusJson, "private-test-password") || strings.Contains(status.StatusJson, pool) {
		t.Fatal("private session leaked into status")
	}
}

func TestTestedProxyInvalidation(t *testing.T) {
	e := newEngine(testHost(), "http://127.0.0.1", time.Hour)
	defer e.Close()
	c := testConfig("http://pool.example:8080", 7)
	c.AutoHarvest = false
	c.HarvestDialProxyMode = "manual"
	c.HarvestDialProxyURL = "http://front.example:3128"
	e.manual = &manualResult{ID: "test", Running: true}
	expires := e.rememberTestedProxy("test", c, "http://session.example:8080", c.HarvestDialProxyURL, time.Now())
	if expires == "" || e.testedProxyRoute(c, c.HarvestDialProxyURL) == "" {
		t.Fatal("missing sample")
	}
	if e.testedProxyRoute(c, "http://changed-front.example:3128") != "" {
		t.Fatal("sample reused after resolved front changed")
	}
	changed := c
	changed.DynamicProxyURL = "http://other-pool.example:8080"
	if e.testedProxyRoute(changed, c.HarvestDialProxyURL) != "" {
		t.Fatal("sample reused for another pool")
	}
	apply(t, e, changed)
	if e.testedProxyRoute(c, c.HarvestDialProxyURL) != "" {
		t.Fatal("network config change did not discard sample")
	}
	e.rememberTestedProxy("test", c, "http://session.example:8080", c.HarvestDialProxyURL, time.Now())
	e.testedProxy.ExpiresAt = time.Now().Add(-time.Second)
	if e.testedProxyRoute(c, c.HarvestDialProxyURL) != "" {
		t.Fatal("expired sample reused")
	}
}

func TestDynamicConnectivityRetriesButUnauthorizedStops(t *testing.T) {
	for _, failure := range []string{"one_exit", "all_exits", "unauthorized", "model_mismatch"} {
		t.Run(failure, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(connectivityFixture))
			defer up.Close()
			var checks, models atomic.Int32
			dynamic := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					n := checks.Add(1)
					if failure == "all_exits" || (failure == "one_exit" && n == 1) {
						w.WriteHeader(http.StatusBadGateway)
						return
					}
				} else {
					models.Add(1)
					if failure == "model_mismatch" {
						completed(w, "gpt-5.6-luna")
						return
					}
					if failure == "unauthorized" {
						w.WriteHeader(http.StatusUnauthorized)
						return
					}
				}
				connectivityFixture(w, r)
			}))
			defer dynamic.Close()
			h := testHost(7)
			e := newEngine(h, up.URL, time.Hour)
			defer e.Close()
			e.exitIPURL = up.URL
			c := testConfig(dynamic.URL, 7)
			c.AutoHarvest = false
			c.MaxAttempts = 2
			c.AttemptIntervalSeconds = 1
			apply(t, e, c)
			e.mu.Lock()
			e.directory[7] = true
			e.mu.Unlock()
			if !action(t, e, manualAction{Kind: "refresh", RequestID: failure, AccountID: 7}).Accepted {
				t.Fatal("refresh rejected")
			}
			wantState := "cooldown"
			if failure == "one_exit" {
				wantState = "ready"
			}
			waitFor(t, e, wantState)
			wantChecks, wantModels, wantError := int32(2), int32(1), ""
			if failure == "all_exits" {
				wantModels, wantError = 0, "dynamic_connectivity_failed"
			} else if failure == "unauthorized" {
				wantChecks, wantError = 1, "upstream_unauthorized"
			} else if failure == "model_mismatch" {
				wantModels, wantError = 2, "model_mismatch"
			}
			e.mu.Lock()
			reason := e.records[keyFor(7, "gpt-6-astra")].LastError
			e.mu.Unlock()
			if checks.Load() != wantChecks || models.Load() != wantModels || reason != wantError {
				t.Fatalf("checks=%d models=%d reason=%s", checks.Load(), models.Load(), reason)
			}
		})
	}
}

func TestManualUnauthorizedIsNotReportedAsConnectivityFailure(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			io.WriteString(w, `{"ip":"203.0.113.8"}`)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer up.Close()
	e := newEngine(testHost(7), up.URL, time.Hour)
	defer e.Close()
	e.exitIPURL = up.URL
	e.mu.Lock()
	e.directory[7] = true
	e.mu.Unlock()
	if !action(t, e, manualAction{Kind: "test", RequestID: "unauthorized", AccountID: 7, Model: "gpt-6-astra", Prompt: "ping"}).Accepted {
		t.Fatal("manual test rejected")
	}
	r := waitManual(t, e)
	if r.HTTPStatus != 401 || r.IPError || r.Complete || !strings.Contains(r.Error, "账号授权被拒绝") || !strings.Contains(r.Error, "已收到上游响应") {
		t.Fatalf("authorization failure misclassified: %+v", r)
	}
}

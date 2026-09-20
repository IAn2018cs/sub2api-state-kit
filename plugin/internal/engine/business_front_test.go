package engine

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Only the front can resolve this synthetic fixed-proxy host. Bypassing the
// front cannot accidentally make a test pass via local DNS or a direct route.
func businessTestFront(t *testing.T, fixed string, hits *atomic.Int32) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		dest := r.Host
		if dest == "fixed.invalid:80" {
			dest = strings.TrimPrefix(fixed, "http://")
		}
		dst, err := net.DialTimeout("tcp", dest, time.Second)
		if err != nil {
			w.WriteHeader(502)
			return
		}
		defer dst.Close()
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		buf.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		buf.Flush()
		done := make(chan struct{})
		go func() { io.Copy(dst, buf); dst.Close(); close(done) }()
		io.Copy(conn, dst)
		conn.Close()
		<-done
	}))
	t.Cleanup(server.Close)
	return server.URL
}
func TestBusinessFrontCollectionAndForward(t *testing.T) {
	var fixedHits, dynamicHits, frontHits atomic.Int32
	fixed := testHTTPProxy(t, &fixedHits, false)
	dynamic := testHTTPProxy(t, &dynamicHits, false)
	front := businessTestFront(t, fixed, &frontHits)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(StateHeader, testState(292))
		completed(w, "gpt-6-astra")
	}))
	defer upstream.Close()
	ip := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{"ip":"203.0.113.8"}`) }))
	defer ip.Close()
	h := testHost(7)
	h.accounts[7].ProxyUrl = "http://fixed.invalid:80"
	e := testEngine(t, h, upstream.URL)
	e.exitIPURL = ip.URL
	c := testConfig(dynamic, 7)
	c.HarvestDialProxyURL = front
	c.BusinessUseFront = true
	c.AutoHarvest = false
	c.ObserveExitIP = true
	apply(t, e, c)
	e.refreshDirectory()
	if !action(t, e, manualAction{Kind: "refresh", AccountID: 7, RequestID: "chain"}).Accepted {
		t.Fatal("manual rejected")
	}
	waitFor(t, e, "ready")
	// Business preflight, dynamic preflight + probe, fixed IP + revalidation.
	if frontHits.Load() < 5 || fixedHits.Load() < 3 || dynamicHits.Load() < 2 {
		t.Fatalf("missing hops front=%d fixed=%d dynamic=%d", frontHits.Load(), fixedHits.Load(), dynamicHits.Load())
	}
	// The same route must work for real forwarding even with STATE disabled.
	e.mu.Lock()
	e.config.Enabled = false
	e.mu.Unlock()
	before := frontHits.Load()
	start := mockStart(upstream.URL, false, 0)
	start.ProxyUrl = h.accounts[7].ProxyUrl
	stream := fixedStream(start, nil)
	if err := e.Forward(stream); err != nil {
		t.Fatal(err)
	}
	ok := false
	for _, f := range stream.frames() {
		if f.GetStart() != nil && f.GetStart().StatusCode == 200 {
			ok = true
		}
	}
	if !ok || frontHits.Load() <= before {
		t.Fatal("business forwarding did not traverse front")
	}
	// Unlisted accounts must retain the original directly reachable proxy route.
	before = frontHits.Load()
	start = mockStart(upstream.URL, false, 0)
	start.AccountId = 8
	start.ProxyUrl = fixed
	stream = fixedStream(start, nil)
	e.Forward(stream)
	if frontHits.Load() != before {
		t.Fatal("unlisted account used the front")
	}
	// Manual baseline works through the front without injecting STATE.
	if !action(t, e, manualAction{Kind: "test", AccountID: 7, Model: "gpt-6-astra", Prompt: "OK", Route: "business", RequestID: "baseline"}).Accepted {
		t.Fatal("test rejected")
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		e.mu.Lock()
		r := e.manual
		done := r != nil && !r.Running
		good := done && r.Complete && !r.Injected && r.Matches
		e.mu.Unlock()
		if done {
			if !good {
				t.Fatal("baseline failed")
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("manual baseline timed out")
}
func TestBusinessFrontScopeAndFailure(t *testing.T) {
	e := testEngine(t, testHost(7), "http://127.0.0.1:1")
	c := DefaultConfig()
	c.BusinessUseFront = true
	c.HarvestDialProxyMode = "managed"
	c.HarvestDialProxyID = 99
	for _, route := range []string{"", "http://127.0.0.1:1234", "socks5://[::1]:1080", "http://localhost:7890"} {
		front, err := e.resolveBusinessFront(context.Background(), c, route)
		if err != nil || front != "" {
			t.Fatal("local bridge changed")
		}
	}
	if _, err := e.resolveBusinessFront(context.Background(), c, "socks5://fixed.invalid:1080"); err == nil {
		t.Fatal("missing front silently bypassed")
	}
	a := AccountConfig{AccountID: 7, Plan: "pro"}
	fp := configFingerprint(c, a, "gpt-test")
	c.BusinessUseFront = false
	if configFingerprint(c, a, "gpt-test") == fp {
		t.Fatal("old route ticket still valid")
	}
	front, err := e.resolveBusinessFront(context.Background(), c, "socks5://fixed.invalid:1080")
	if err != nil || front != "" {
		t.Fatal("default routing changed")
	}
}

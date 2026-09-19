package engine

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	pluginv1 "github.com/wangyunjeff/sub2api-state-kit/plugin/internal/pluginapi/v1"
)

// Tiny real TCP tunnel fixtures exercise all hops; no Internet is used.
func testHTTPProxy(t *testing.T, hits *atomic.Int32, expectAuth bool) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if expectAuth && r.Header.Get("Proxy-Authorization") != "Basic ZnJvbnQ6c2VjcmV0" {
			t.Error("missing front authentication")
			w.WriteHeader(407)
			return
		}
		if !expectAuth && r.Header.Get("Proxy-Authorization") != "" {
			t.Error("front credentials leaked to inner")
		}
		if r.Method == http.MethodConnect {
			dst, err := net.Dial("tcp", r.Host)
			if err != nil {
				w.WriteHeader(502)
				return
			}
			conn, buf, err := w.(http.Hijacker).Hijack()
			if err != nil {
				dst.Close()
				return
			}
			defer conn.Close()
			defer dst.Close()
			buf.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
			buf.Flush()
			done := make(chan struct{})
			go func() { io.Copy(dst, buf); dst.Close(); close(done) }()
			io.Copy(conn, dst)
			conn.Close()
			<-done
			return
		}
		req := r.Clone(r.Context())
		req.RequestURI = ""
		req.Header.Del("Proxy-Authorization")
		resp, err := http.DefaultTransport.RoundTrip(req)
		if err != nil {
			w.WriteHeader(502)
			return
		}
		defer resp.Body.Close()
		for k, v := range resp.Header {
			w.Header()[k] = v
		}
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
	}))
	t.Cleanup(server.Close)
	if expectAuth {
		return strings.Replace(server.URL, "http://", "http://front:secret@", 1)
	}
	return server.URL
}
func testSOCKSProxy(t *testing.T, hits *atomic.Int32) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				c.SetDeadline(time.Now().Add(10 * time.Second))
				b := make([]byte, 2)
				if _, err := io.ReadFull(c, b); err != nil {
					return
				}
				methods := make([]byte, int(b[1]))
				if _, err := io.ReadFull(c, methods); err != nil {
					return
				}
				c.Write([]byte{5, 0})
				h := make([]byte, 4)
				if _, err := io.ReadFull(c, h); err != nil {
					return
				}
				host := ""
				switch h[3] {
				case 1:
					a := make([]byte, 4)
					io.ReadFull(c, a)
					host = net.IP(a).String()
				case 3:
					n := make([]byte, 1)
					io.ReadFull(c, n)
					a := make([]byte, int(n[0]))
					io.ReadFull(c, a)
					host = string(a)
				case 4:
					a := make([]byte, 16)
					io.ReadFull(c, a)
					host = net.IP(a).String()
				default:
					return
				}
				p := make([]byte, 2)
				if _, err := io.ReadFull(c, p); err != nil {
					return
				}
				dst, err := net.Dial("tcp", net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(p)))))
				if err != nil {
					return
				}
				defer dst.Close()
				hits.Add(1)
				c.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 0})
				done := make(chan struct{})
				go func() { io.Copy(dst, c); dst.Close(); close(done) }()
				io.Copy(c, dst)
				c.Close()
				<-done
			}()
		}
	}()
	return "socks5h://" + ln.Addr().String()
}
func TestHarvestChainRealHops(t *testing.T) {
	for _, outerKind := range []string{"http", "socks"} {
		for _, innerKind := range []string{"http", "socks"} {
			t.Run(outerKind+"-"+innerKind, func(t *testing.T) {
				var outerHits, innerHits, targetHits atomic.Int32
				target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					targetHits.Add(1)
					if r.Header.Get("Proxy-Authorization") != "" {
						t.Error("proxy auth at target")
					}
					io.WriteString(w, "success")
				}))
				defer target.Close()
				outer := testHTTPProxy(t, &outerHits, true)
				if outerKind == "socks" {
					outer = testSOCKSProxy(t, &outerHits)
				}
				inner := testHTTPProxy(t, &innerHits, false)
				if innerKind == "socks" {
					inner = testSOCKSProxy(t, &innerHits)
				}
				client, err := harvestClient(inner, outer)
				if err != nil {
					t.Fatal(err)
				}
				defer client.CloseIdleConnections()
				resp, err := client.Get(target.URL)
				if err != nil {
					t.Fatal(err)
				}
				b, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				if string(b) != "success" || outerHits.Load() != 1 || innerHits.Load() != 1 || targetHits.Load() != 1 {
					t.Fatalf("wrong route %q %d/%d/%d", b, outerHits.Load(), innerHits.Load(), targetHits.Load())
				}
			})
		}
	}
}
func TestFrontProxyFailureNeverFallsBack(t *testing.T) {
	var hits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer target.Close()
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(407) }))
	defer front.Close()
	client, err := harvestClient(target.URL, front.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = client.Get(target.URL); err == nil {
		t.Fatal("front rejection accepted")
	}
	if hits.Load() != 0 {
		t.Fatal("fell back to direct")
	}
}
func TestFrontProxyCancellation(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		io.Copy(io.Discard, c)
	}()
	client, err := harvestClient("http://unresolvable.invalid:1234", "http://"+ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://origin.invalid", nil)
	start := time.Now()
	_, err = client.Do(req)
	if err == nil || time.Since(start) > time.Second {
		t.Fatal("cancellation not effective")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("tunnel leaked")
	}
}
func TestChainedCollectionLogsAndFixedIsolation(t *testing.T) {
	var frontHits, innerHits, fixedHits, ipHits atomic.Int32
	front := testHTTPProxy(t, &frontHits, true)
	dynamic := testHTTPProxy(t, &innerHits, false)
	fixed := testHTTPProxy(t, &fixedHits, false)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(StateHeader, testState(292))
		completed(w, "gpt-6-astra")
	}))
	defer upstream.Close()
	ip := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ipHits.Add(1)
		if r.Header.Get("Authorization") != "" || r.Header.Get("Chatgpt-Account-Id") != "" || r.Header.Get(StateHeader) != "" {
			t.Error("account credentials leaked to IP check")
		}
		io.WriteString(w, `{"ip":"203.0.113.8"}`)
	}))
	defer ip.Close()
	h := testHost(7)
	h.accounts[7].ProxyUrl = fixed
	e := testEngine(t, h, upstream.URL)
	e.exitIPURL = ip.URL
	c := testConfig(dynamic, 7)
	c.HarvestDialProxyURL = front
	c.ObserveExitIP = true
	apply(t, e, c)
	waitFor(t, e, "ready")
	if frontHits.Load() != 2 || innerHits.Load() != 2 || fixedHits.Load() != 2 || ipHits.Load() != 2 {
		t.Fatalf("wrong routing %d/%d/%d/%d", frontHits.Load(), innerHits.Load(), fixedHits.Load(), ipHits.Load())
	}
	health, _ := e.Health(context.Background(), &pluginv1.HealthRequest{})
	if strings.Contains(health.StatusJson, "secret") || strings.Contains(health.StatusJson, "test-token") || strings.Contains(health.StatusJson, testState(292)) {
		t.Fatal("secret leaked")
	}
	var snap statusSnapshot
	json.Unmarshal([]byte(health.StatusJson), &snap)
	found := false
	for _, event := range snap.Events {
		if event.Phase == "validate" && event.Result == "model_matched" && event.ExitIP == "203.0.113.8" && event.ActualModel == "gpt-6-astra" && !event.Chained {
			found = true
		}
	}
	if !found {
		t.Fatal("missing verified activity")
	}
}
func TestExitIPFailureNonFatalAndEventsBounded(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(StateHeader, testState(292))
		completed(w, "gpt-6-astra")
	}))
	defer upstream.Close()
	var hits atomic.Int32
	e := testEngine(t, testHost(7), upstream.URL)
	e.exitIPURL = upstream.URL
	c := testConfig(testHTTPProxy(t, &hits, false), 7)
	c.ObserveExitIP = true
	apply(t, e, c)
	waitFor(t, e, "ready")
	e.mu.Lock()
	for i := 0; i < maxActivityEvents+20; i++ {
		e.eventLocked(activityEvent{AccountID: 7, Model: "gpt-test", Phase: "harvest", Result: "started"})
	}
	n := len(e.events)
	e.mu.Unlock()
	if n != maxActivityEvents {
		t.Fatal(n)
	}
}
func TestFrontProxyConfigAndFingerprint(t *testing.T) {
	c := testConfig("http://dynamic.example:8080", 7)
	a := c.Accounts[0]
	old := configFingerprint(c, a, "gpt-test")
	c.HarvestDialProxyURL = "socks5h://outer.example:1080"
	if _, err := ParseConfig([]byte(jsonText(c))); err != nil {
		t.Fatal(err)
	}
	if old == configFingerprint(c, a, "gpt-test") {
		t.Fatal("front route not bound")
	}
	for _, bad := range []string{"file:///tmp/private", "http://user-{sid}:secret@proxy.example", "http://proxy.example?secret=1"} {
		c.HarvestDialProxyURL = bad
		if _, err := ParseConfig([]byte(jsonText(c))); err == nil {
			t.Fatal("bad outer accepted")
		}
	}
}

func TestTransportErrorClassificationNeverIncludesCredentials(t *testing.T) {
	for _, tc := range []struct{ message, code string }{
		{"socks5://private:secret@proxy: username/password authentication failed", "proxy_auth_failed"},
		{"front proxy CONNECT rejected", "front_proxy_failed"},
		{"x509: certificate error private-secret", "transport_tls_failed"},
		{"unknown private-secret", "transport_failed"},
	} {
		if got := transportCode(errors.New(tc.message)); got != tc.code {
			t.Fatalf("wrong code %s", got)
		}
	}
}

func TestHarvestChainTLSOrigin(t *testing.T) {
	for _, kind := range []string{"http", "socks"} {
		t.Run(kind, func(t *testing.T) {
			var frontHits, innerHits atomic.Int32
			target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "tls-origin") }))
			defer target.Close()
			inner := testHTTPProxy(t, &innerHits, false)
			if kind == "socks" {
				inner = testSOCKSProxy(t, &innerHits)
			}
			client, err := harvestClient(inner, testHTTPProxy(t, &frontHits, true))
			if err != nil {
				t.Fatal(err)
			}
			// Trust this test origin certificate only; production uses system trust roots.
			client.Transport.(*harvestTransport).base.TLSClientConfig = target.Client().Transport.(*http.Transport).TLSClientConfig
			resp, err := client.Get(target.URL)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			if string(b) != "tls-origin" || frontHits.Load() != 1 || innerHits.Load() != 1 {
				t.Fatal("TLS route failed")
			}
		})
	}
}

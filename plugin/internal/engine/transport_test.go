package engine

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pluginv1 "github.com/wangyunjeff/sub2api-state-kit/plugin/internal/pluginapi/v1"
	"google.golang.org/grpc/metadata"
)

type mockForwardStream struct {
	ctx    context.Context
	in     chan *pluginv1.ForwardRequest
	mu     sync.Mutex
	out    []*pluginv1.ForwardResponse
	onSend func(*pluginv1.ForwardResponse)
}

func (s *mockForwardStream) Context() context.Context   { return s.ctx }
func (*mockForwardStream) SetHeader(metadata.MD) error  { return nil }
func (*mockForwardStream) SendHeader(metadata.MD) error { return nil }
func (*mockForwardStream) SetTrailer(metadata.MD)       {}
func (*mockForwardStream) SendMsg(any) error            { return nil }
func (*mockForwardStream) RecvMsg(any) error            { return nil }
func (s *mockForwardStream) Recv() (*pluginv1.ForwardRequest, error) {
	select {
	case f, ok := <-s.in:
		if !ok {
			return nil, io.EOF
		}
		return f, nil
	case <-s.ctx.Done():
		return nil, s.ctx.Err()
	}
}
func (s *mockForwardStream) Send(f *pluginv1.ForwardResponse) error {
	if err := s.ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	s.out = append(s.out, f)
	s.mu.Unlock()
	if s.onSend != nil {
		s.onSend(f)
	}
	return nil
}
func (s *mockForwardStream) frames() []*pluginv1.ForwardResponse {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*pluginv1.ForwardResponse(nil), s.out...)
}
func mockStart(url string, body bool, length int64) *pluginv1.ForwardRequestStart {
	return &pluginv1.ForwardRequestStart{Method: http.MethodPost, Url: url, AccountId: 7, Platform: "openai", AccountType: "oauth", HasBody: body, ContentLength: length, Headers: map[string]*pluginv1.HeaderValues{}}
}
func frameStart(s *pluginv1.ForwardRequestStart) *pluginv1.ForwardRequest {
	return &pluginv1.ForwardRequest{Frame: &pluginv1.ForwardRequest_Start{Start: s}}
}
func frameBody(b []byte) *pluginv1.ForwardRequest {
	return &pluginv1.ForwardRequest{Frame: &pluginv1.ForwardRequest_BodyChunk{BodyChunk: b}}
}
func frameEnd() *pluginv1.ForwardRequest {
	return &pluginv1.ForwardRequest{Frame: &pluginv1.ForwardRequest_BodyEnd{BodyEnd: true}}
}
func fixedStream(start *pluginv1.ForwardRequestStart, body []byte) *mockForwardStream {
	s := &mockForwardStream{ctx: context.Background(), in: make(chan *pluginv1.ForwardRequest, 3)}
	s.in <- frameStart(start)
	if start.HasBody {
		s.in <- frameBody(body)
		s.in <- frameEnd()
	}
	close(s.in)
	return s
}
func forwardingEngine(t *testing.T, enabled bool) *Engine {
	c := DefaultConfig()
	c.AllowWithoutTicket = false
	c.Enabled = enabled
	c.Accounts = []AccountConfig{{AccountID: 7, Enabled: true, Plan: "pro", Models: []string{"gpt-test"}}}
	e := &Engine{config: c, clients: newClientPool(), directory: map[int64]bool{7: true}, hostReady: true, tickets: map[string]*ticket{}, revoked: map[string]string{}, records: map[string]*jobRecord{}, wake: make(chan struct{}, 1)}
	t.Cleanup(e.clients.Close)
	return e
}
func addForwardTicket(e *Engine, proxy string) string {
	state := "gAAAAA" + strings.Repeat("a", 286)
	a := e.config.Accounts[0]
	e.tickets[keyFor(7, "gpt-test")] = &ticket{AccountID: 7, Model: "gpt-test", Plan: "pro", State: state, Version: "test-version", ConfigFingerprint: configFingerprint(e.config, a, "gpt-test"), FixedFingerprint: proxyFingerprint(proxy), IdentityFingerprint: stableHeaders(7, nil), CapturedAt: time.Now().Add(-time.Minute), ExpiresAt: time.Now().Add(20 * time.Minute)}
	return state
}

func TestForwardPreservesHeadersAndRawResponse(t *testing.T) {
	requestBody := []byte("arbitrary non-JSON passthrough body")
	responseBody := []byte{0x1f, 0x8b, 0x08, 0x00, 0xff, 0x09}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read: %v", err)
		}
		if !bytes.Equal(body, requestBody) || r.ContentLength != int64(len(requestBody)) {
			t.Error("body or length changed")
		}
		if len(r.Header.Values("X-Repeat")) != 2 {
			t.Error("request headers collapsed")
		}
		if r.Header.Get(StateHeader) != "caller-value" {
			t.Error("off mode altered client state")
		}
		w.Header().Add("Set-Cookie", "first=a")
		w.Header().Add("Set-Cookie", "second=b")
		w.Header().Set("Content-Encoding", "gzip")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write(responseBody)
	}))
	defer server.Close()
	start := mockStart(server.URL, true, int64(len(requestBody)))
	start.Headers["X-Repeat"] = &pluginv1.HeaderValues{Values: []string{"one", "two"}}
	start.Headers[StateHeader] = &pluginv1.HeaderValues{Values: []string{"caller-value"}}
	s := fixedStream(start, requestBody)
	e := forwardingEngine(t, false)
	if err := e.Forward(s); err != nil {
		t.Fatal(err)
	}
	frames := s.frames()
	if len(frames) < 3 || frames[0].GetStart().StatusCode != 202 {
		t.Fatalf("unexpected response: %v", frames)
	}
	if got := frames[0].GetStart().Headers["Set-Cookie"]; got == nil || len(got.Values) != 2 {
		t.Fatal("repeated response header lost")
	}
	var got []byte
	for _, f := range frames {
		got = append(got, f.GetBodyChunk()...)
	}
	if !bytes.Equal(got, responseBody) {
		t.Fatalf("response bytes changed: %x", got)
	}
	if frames[len(frames)-1].GetEnd().BytesReceived != int64(len(responseBody)) {
		t.Fatal("incorrect byte count")
	}
}

func TestForwardStreamsRequestAndResponseBeforeCompletion(t *testing.T) {
	firstSeen := make(chan struct{})
	releaseResponse := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, 5)
		if _, err := io.ReadFull(r.Body, b); err != nil {
			t.Errorf("first body: %v", err)
			return
		}
		close(firstSeen)
		if string(b) != "first" {
			t.Error("wrong first chunk")
		}
		rest, _ := io.ReadAll(r.Body)
		if string(rest) != "second" {
			t.Error("wrong second chunk")
		}
		_, _ = io.WriteString(w, "chunk-one")
		w.(http.Flusher).Flush()
		<-releaseResponse
		_, _ = io.WriteString(w, "chunk-two")
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	seenResponse := make(chan struct{}, 1)
	s := &mockForwardStream{ctx: ctx, in: make(chan *pluginv1.ForwardRequest, 4), onSend: func(f *pluginv1.ForwardResponse) {
		if len(f.GetBodyChunk()) > 0 {
			select {
			case seenResponse <- struct{}{}:
			default:
			}
		}
	}}
	s.in <- frameStart(mockStart(server.URL, true, -1))
	s.in <- frameBody([]byte("first"))
	e := forwardingEngine(t, false)
	done := make(chan error, 1)
	go func() { done <- e.Forward(s) }()
	select {
	case <-firstSeen:
	case <-ctx.Done():
		close(releaseResponse)
		t.Fatal("request was buffered")
	}
	s.in <- frameBody([]byte("second"))
	s.in <- frameEnd()
	close(s.in)
	select {
	case <-seenResponse:
	case <-ctx.Done():
		close(releaseResponse)
		t.Fatal("response was buffered")
	}
	close(releaseResponse)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	var response []byte
	for _, f := range s.frames() {
		response = append(response, f.GetBodyChunk()...)
	}
	if string(response) != "chunk-onechunk-two" {
		t.Fatalf("wrong output %q", response)
	}
}

func TestConfiguredAccountFailsClosedWithoutTicket(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer server.Close()
	for _, body := range [][]byte{[]byte(`{"model":"gpt-test"}`), []byte("invalid json"), bytes.Repeat([]byte{'x'}, maxTicketRequestBody+1)} {
		e := forwardingEngine(t, true)
		s := fixedStream(mockStart(server.URL, true, int64(len(body))), body)
		if err := e.Forward(s); err != nil {
			t.Fatal(err)
		}
		frames := s.frames()
		if len(frames) != 3 || frames[0].GetStart().StatusCode != 503 || frames[2].GetEnd() == nil {
			t.Fatal("must return synthetic503 start/body/end")
		}
		for _, f := range frames {
			if f.GetError() != nil {
				t.Fatal("synthetic503 incorrectly includes error frame")
			}
		}
	}
	if hits.Load() != 0 {
		t.Fatal("configured request reached upstream without verified ticket")
	}
}

func TestForwardOnlyProtectsConfiguredModelsAndAccounts(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		if r.Header.Get(StateHeader) != "" {
			t.Error("unexpected injected state")
		}
		_, _ = io.WriteString(w, "ok")
	}))
	defer server.Close()
	tests := []struct {
		account     int64
		model       string
		accountType string
	}{{8, "", "oauth"}, {7, "gpt-other", "oauth"}, {7, "", "apikey"}}
	for _, tc := range tests {
		body := []byte(fmt.Sprintf(`{"model":%q}`, tc.model))
		start := mockStart(server.URL, true, int64(len(body)))
		start.AccountId = tc.account
		start.AccountType = tc.accountType
		e := forwardingEngine(t, true)
		if err := e.Forward(fixedStream(start, body)); err != nil {
			t.Fatal(err)
		}
	}
	if hits.Load() != 3 {
		t.Fatal("normal account/model forwarding was gated")
	}
}

func TestEnabledAccountUnlistedModelAcceptsLargeBody(t *testing.T) {
	body := []byte(`{"model":"gpt-other","input":"` + strings.Repeat("a", 2<<20) + `"}`)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, err := io.ReadAll(r.Body)
		if err != nil || !bytes.Equal(got, body) {
			t.Error("large unlisted-model request was altered")
		}
		_, _ = io.WriteString(w, "ok")
	}))
	defer server.Close()
	e := forwardingEngine(t, true)
	s := fixedStream(mockStart(server.URL, true, int64(len(body))), body)
	if err := e.Forward(s); err != nil {
		t.Fatal(err)
	}
	if frames := s.frames(); frames[0].GetStart().StatusCode != 200 {
		t.Fatal("large unlisted model was gated")
	}
}

func TestForwardInjectsTicketAndInvalidatesOnModelMismatch(t *testing.T) {
	e := forwardingEngine(t, true)
	state := addForwardTicket(e, "")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		values := r.Header.Values(StateHeader)
		if len(values) != 1 || values[0] != state {
			t.Error("ticket replacement failed")
		}
		if values := r.Header.Values("Accept-Encoding"); len(values) != 1 || values[0] != "identity" {
			t.Error("ticket response did not negotiate inspectable uncompressed bytes")
		}
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"model\":\"gpt-other\"}}\n\n")
	}))
	defer server.Close()
	body := []byte(`{"model":"gpt-test"}`)
	start := mockStart(server.URL, true, int64(len(body)))
	start.Headers[StateHeader] = &pluginv1.HeaderValues{Values: []string{"old"}}
	start.Headers["X-Codex-Turn-State"] = &pluginv1.HeaderValues{Values: []string{"other-old"}}
	start.Headers["Accept-Encoding"] = &pluginv1.HeaderValues{Values: []string{"gzip"}}
	start.Headers["accept-encoding"] = &pluginv1.HeaderValues{Values: []string{"br"}}
	if err := e.Forward(fixedStream(start, body)); err != nil {
		t.Fatal(err)
	}
	record := e.records[keyFor(7, "gpt-test")]
	if e.tickets[keyFor(7, "gpt-test")] != nil || record == nil || record.LastError != "model_mismatch" {
		t.Fatal("mismatched completion did not invalidate ticket")
	}
}

func TestForwardInvalidatesCompletedMismatchBeforeEOF(t *testing.T) {
	e := forwardingEngine(t, true)
	addForwardTicket(e, "")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"model\":\"gpt-other\"}}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	body := []byte(`{"model":"gpt-test"}`)
	s := fixedStream(mockStart(server.URL, true, int64(len(body))), body)
	s.ctx = ctx
	s.onSend = func(f *pluginv1.ForwardResponse) {
		if len(f.GetBodyChunk()) > 0 {
			cancel()
		}
	}
	_ = e.Forward(s)
	record := e.records[keyFor(7, "gpt-test")]
	if e.tickets[keyFor(7, "gpt-test")] != nil || record == nil || record.LastError != "model_mismatch" {
		t.Fatal("completed mismatch was missed when consumer stopped before EOF")
	}
}

func TestForwardInvalidatesState312ButNotErrorResponses(t *testing.T) {
	for _, status := range []int{200, 401} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			e := forwardingEngine(t, true)
			addForwardTicket(e, "")
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set(StateHeader, "gAAAAA"+strings.Repeat("b", 306))
				w.WriteHeader(status)
				_, _ = io.WriteString(w, "{}")
			}))
			defer server.Close()
			body := []byte(`{"model":"gpt-test"}`)
			if err := e.Forward(fixedStream(mockStart(server.URL, true, int64(len(body))), body)); err != nil {
				t.Fatal(err)
			}
			_, exists := e.tickets[keyFor(7, "gpt-test")]
			if exists != (status == 401) {
				t.Fatalf("incorrect invalidation at status%d", status)
			}
		})
	}
}

func TestForwardUpstreamReadErrorIsSanitized(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		_, _ = io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Length: 20\r\n\r\nshort")
	}))
	defer server.Close()
	e := forwardingEngine(t, false)
	s := fixedStream(mockStart(server.URL+"?secret=do-not-echo", false, 0), nil)
	if err := e.Forward(s); err != nil {
		t.Fatal(err)
	}
	frames := s.frames()
	last := frames[len(frames)-1].GetError()
	if last == nil || !last.RequestSent || last.Code != "upstream_read" || strings.Contains(last.Message, "secret") {
		t.Fatalf("incorrect read error: %v", last)
	}
}

func TestForwardCancellationStopsTransport(t *testing.T) {
	entered := make(chan struct{})
	exited := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(entered); <-r.Context().Done(); close(exited) }))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := fixedStream(mockStart(server.URL, false, 0), nil)
	s.ctx = ctx
	e := forwardingEngine(t, false)
	done := make(chan error, 1)
	go func() { done <- e.Forward(s) }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("request did not start")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Forward ignored cancellation")
	}
	select {
	case <-exited:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream context was not canceled")
	}
}

func TestForwardShutdownCancelsStalledConfiguredBody(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e := forwardingEngine(t, true)
	e.ctx = ctx
	streamCtx, streamCancel := context.WithCancel(context.Background())
	defer streamCancel()
	s := &mockForwardStream{ctx: streamCtx, in: make(chan *pluginv1.ForwardRequest, 1)}
	s.in <- frameStart(mockStart("http://127.0.0.1:1", true, -1))
	done := make(chan error, 1)
	go func() { done <- e.Forward(s) }()
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("engine shutdown ignored while receiving body")
	}
}

func TestForwardDoesNotFollowRedirectOrDisableTLSVerification(t *testing.T) {
	var targetHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { targetHits.Add(1) }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	e := forwardingEngine(t, false)
	s := fixedStream(mockStart(redirect.URL, false, 0), nil)
	if err := e.Forward(s); err != nil {
		t.Fatal(err)
	}
	if targetHits.Load() != 0 || s.frames()[0].GetStart().StatusCode != 307 {
		t.Fatal("business request followed redirect")
	}
	tlsServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("self-signed TLS accepted") }))
	defer tlsServer.Close()
	s = fixedStream(mockStart(tlsServer.URL, false, 0), nil)
	if err := e.Forward(s); err != nil {
		t.Fatal(err)
	}
	frames := s.frames()
	if len(frames) != 1 || frames[0].GetError() == nil || !frames[0].GetError().RequestSent {
		t.Fatal("invalid TLS was not rejected")
	}
}

func TestForwardInvalidProxyIsNotSentAndDoesNotLeak(t *testing.T) {
	start := mockStart("http://127.0.0.1:1", false, 0)
	start.ProxyUrl = "bogus://user:private-secret@example.test"
	s := fixedStream(start, nil)
	e := forwardingEngine(t, false)
	if err := e.Forward(s); err != nil {
		t.Fatal(err)
	}
	frames := s.frames()
	if len(frames) != 1 {
		t.Fatal("unexpected frames")
	}
	failure := frames[0].GetError()
	if failure == nil || failure.RequestSent || strings.Contains(failure.Message, "private-secret") {
		t.Fatal("invalid proxy error leaks or claims sent")
	}
}

func TestClientPoolBoundedAndProbeClientsAreFresh(t *testing.T) {
	p := newClientPool()
	defer p.Close()
	first, err := p.client("")
	if err != nil {
		t.Fatal(err)
	}
	second, _ := p.client("")
	if first != second {
		t.Fatal("client not reused")
	}
	for i := 0; i < maxPooledClients+20; i++ {
		if _, err := p.client(fmt.Sprintf("http://user:pass@proxy-%d.invalid:80", i)); err != nil {
			t.Fatal(err)
		}
	}
	if len(p.clients) != maxPooledClients {
		t.Fatal("pool is not bounded")
	}
	for _, scheme := range []string{"http", "https", "socks5", "socks5h"} {
		client, err := freshProbeClient(scheme + "://localhost:1234")
		if err != nil {
			t.Fatal(err)
		}
		transport := client.Transport.(*http.Transport)
		if !transport.DisableKeepAlives || !transport.DisableCompression || transport.TLSClientConfig != nil && transport.TLSClientConfig.InsecureSkipVerify {
			t.Fatal("unsafe probe transport")
		}
		if err := client.CheckRedirect(nil, nil); err != http.ErrUseLastResponse {
			t.Fatal("redirect enabled")
		}
		if transport.MaxConnsPerHost != 0 {
			t.Fatal("transport introduced an account concurrency cap")
		}
	}
}

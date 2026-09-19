package engine

import (
	"context"
	"encoding/json"
	"errors"
	pluginv1 "github.com/wangyunjeff/sub2api-state-kit/plugin/internal/pluginapi/v1"
	"google.golang.org/grpc"
	"strings"
	"testing"
	"time"
)

type resourceHost struct {
	*fakeHost
	url       string
	found     bool
	err       error
	requested int64
}

func (h *resourceHost) ListResources(context.Context, *pluginv1.ListResourcesRequest, ...grpc.CallOption) (*pluginv1.ListResourcesResponse, error) {
	return &pluginv1.ListResourcesResponse{Accounts: []*pluginv1.AccountSummary{{Id: 7, Name: "Mail Pro"}}, Proxies: []*pluginv1.ProxySummary{{Id: 3, Name: "Front", Host: "front.test", Port: 8080, Protocol: "http"}}}, nil
}
func (h *resourceHost) ResolveProxy(_ context.Context, r *pluginv1.ResolveProxyRequest, _ ...grpc.CallOption) (*pluginv1.ResolveProxyResponse, error) {
	h.requested = r.ProxyId
	return &pluginv1.ResolveProxyResponse{Found: h.found, ProxyUrl: h.url}, h.err
}
func TestResourceDirectoryAndStockFallback(t *testing.T) {
	for _, supported := range []bool{true, false} {
		var host pluginv1.HostServiceClient = testHost(7)
		if supported {
			host = &resourceHost{fakeHost: testHost(7)}
		}
		e := newEngine(host, "http://unused.test", time.Hour)
		e.refreshDirectory()
		st, _ := e.Health(context.Background(), &pluginv1.HealthRequest{})
		var snap statusSnapshot
		json.Unmarshal([]byte(st.StatusJson), &snap)
		if snap.ResourcesReady != supported || len(snap.AccountIDs) != 1 {
			t.Fatal("catalog fallback failed")
		}
		if supported && (len(snap.Accounts) != 1 || snap.Accounts[0].Name != "Mail Pro") {
			t.Fatal("missing names")
		}
		e.Close()
	}
}
func TestManagedProxyResolutionFreshAndFailClosed(t *testing.T) {
	h := &resourceHost{fakeHost: testHost(7), url: "http://user:private-password@front.test:8080", found: true}
	e := newEngine(h, "http://unused.test", time.Hour)
	defer e.Close()
	c := DefaultConfig()
	c.HarvestDialProxyMode = "managed"
	c.HarvestDialProxyID = 3
	u, err := e.resolveFrontProxy(context.Background(), c)
	if err != nil || u != h.url || h.requested != 3 {
		t.Fatal("managed proxy not resolved")
	}
	h.url = "socks5h://updated.test:1080"
	u, err = e.resolveFrontProxy(context.Background(), c)
	if err != nil || u != h.url {
		t.Fatal("stale proxy cached")
	}
	for _, v := range []string{"", "file:///secret", "http://{sid}.test"} {
		h.url = v
		_, err = e.resolveFrontProxy(context.Background(), c)
		if !errors.Is(err, errManagedProxyUnavailable) {
			t.Fatal("invalid proxy did not fail closed")
		}
	}
	h.url = "http://valid.test"
	h.found = false
	if _, err = e.resolveFrontProxy(context.Background(), c); !errors.Is(err, errManagedProxyUnavailable) {
		t.Fatal("missing proxy fell back")
	}
	h.found = true
	h.err = errors.New("private-password")
	_, err = e.resolveFrontProxy(context.Background(), c)
	if err == nil || strings.Contains(err.Error(), "private-password") {
		t.Fatal("raw host error leaked")
	}
	c.HarvestDialProxyMode = "direct"
	c.HarvestDialProxyURL = "http://ignored.test"
	if u, err = e.resolveFrontProxy(context.Background(), c); err != nil || u != "" {
		t.Fatal("direct is not direct")
	}
}
func TestManagedConfigNormalizationAndFingerprint(t *testing.T) {
	c := testConfig("http://dynamic.test", 7)
	a := c.Accounts[0]
	original := configFingerprint(c, a, "gpt-test")
	for _, mode := range []string{"direct", "manual", "managed"} {
		c.HarvestDialProxyMode = mode
		c.HarvestDialProxyURL = "http://front.test"
		c.HarvestDialProxyID = 3
		got, err := ParseConfig([]byte(jsonText(c)))
		if err != nil {
			t.Fatal(err)
		}
		if mode != "manual" && got.HarvestDialProxyURL != "" {
			t.Fatal("unused credential retained")
		}
		if mode != "managed" && got.HarvestDialProxyID != 0 {
			t.Fatal("unused ID retained")
		}
		if mode == "direct" && configFingerprint(got, a, "gpt-test") != original {
			t.Fatal("direct compatibility lost")
		}
	}
	c.HarvestDialProxyMode = "managed"
	c.HarvestDialProxyID = 0
	if _, err := ParseConfig([]byte(jsonText(c))); err == nil {
		t.Fatal("missing ID accepted")
	}
	c.HarvestDialProxyID = 3
	fp := configFingerprint(c, a, "gpt-test")
	c.HarvestDialProxyID = 4
	if fp == configFingerprint(c, a, "gpt-test") {
		t.Fatal("selected ID not bound")
	}
}

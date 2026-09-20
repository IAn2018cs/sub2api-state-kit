package engine

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type countryEntry struct {
	Code    string
	Expires time.Time
}

func validCountryCode(code string) bool {
	return len(code) == 2 && code[0] >= 'A' && code[0] <= 'Z' && code[1] >= 'A' && code[1] <= 'Z' && code != "XX" && code != "ZZ"
}

// Look up the IP actually observed, never the geography of the lookup connection.
// This separate request sends only the public IP, with no OAuth or proxy credentials.
func (e *Engine) countryForIP(ctx context.Context, raw string) string {
	ip := net.ParseIP(raw)
	if ip == nil || !ip.IsGlobalUnicast() || ip.IsPrivate() {
		return ""
	}
	raw = ip.String()
	e.mu.Lock()
	endpoint := e.countryURL
	cached, ok := e.countryCache[raw]
	e.mu.Unlock()
	if ok && time.Now().Before(cached.Expires) {
		return cached.Code
	}
	if endpoint == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(endpoint, "/")+"/"+url.PathEscape(raw), nil)
	if err != nil {
		return ""
	}
	code := ""
	if response, err := e.countryClient.Do(req); err == nil {
		b, readErr := io.ReadAll(io.LimitReader(response.Body, 4097))
		response.Body.Close()
		var data struct {
			IP      string `json:"ip"`
			Country string `json:"country"`
		}
		if response.StatusCode == 200 && readErr == nil && len(b) <= 4096 && json.Unmarshal(b, &data) == nil && ip.Equal(net.ParseIP(data.IP)) && validCountryCode(data.Country) {
			code = data.Country
		}
	}
	ttl := 6 * time.Hour
	if code == "" {
		ttl = time.Minute
	}
	e.mu.Lock()
	if len(e.countryCache) >= 512 {
		for key := range e.countryCache {
			delete(e.countryCache, key)
			break
		}
	}
	e.countryCache[raw] = countryEntry{code, time.Now().Add(ttl)}
	e.mu.Unlock()
	return code
}

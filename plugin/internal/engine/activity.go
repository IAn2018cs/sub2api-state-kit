package engine

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	pluginv1 "github.com/wangyunjeff/sub2api-state-kit/plugin/internal/pluginapi/v1"
)

const maxActivityEvents = 200

var errConnectivityCheck = errors.New("proxy connectivity check failed")

var errManagedProxyUnavailable = errors.New("managed proxy unavailable")

// Deliberately no free-form error, URL, headers, body, credential or ticket.
type activityEvent struct {
	ReusedProxySession bool   `json:"reused_proxy_session,omitempty"`
	RoundID            string `json:"round_id,omitempty"`
	Trigger            string `json:"trigger,omitempty"`
	Route              string `json:"route,omitempty"`
	CountryCode        string `json:"country_code,omitempty"`
	ID                 uint64 `json:"id"`
	Time               string `json:"time"`
	AccountID          int64  `json:"account_id"`
	Model              string `json:"model"`
	Attempt            int    `json:"attempt"`
	Phase              string `json:"phase"`
	Result             string `json:"result"`
	ExitIP             string `json:"exit_ip,omitempty"`
	ActualModel        string `json:"actual_model,omitempty"`
	HTTPStatus         int    `json:"http_status,omitempty"`
	StateBytes         int    `json:"state_bytes,omitempty"`
	DurationMS         int64  `json:"duration_ms,omitempty"`
	Chained            bool   `json:"chained"`
}

func (e *Engine) eventLocked(event activityEvent) {
	e.eventSeq++
	event.ID = e.eventSeq
	event.Time = time.Now().UTC().Format(time.RFC3339)
	if !modelPattern.MatchString(event.Model) {
		event.Model = ""
	}
	if !modelPattern.MatchString(event.ActualModel) {
		event.ActualModel = ""
	}
	if !validCountryCode(event.CountryCode) {
		event.CountryCode = ""
	}
	if net.ParseIP(event.ExitIP) == nil {
		event.ExitIP = ""
	}
	if len(e.events) >= maxActivityEvents {
		copy(e.events, e.events[1:])
		e.events = e.events[:maxActivityEvents-1]
	}
	e.events = append(e.events, event)
	log.Print("state-kit activity " + jsonText(event)) // stderr, never plugin protocol stdout
}
func (e *Engine) activity(k string, gen uint64, event activityEvent) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.activeLocked(k, gen) {
		if r := e.records[k]; r != nil {
			event.Trigger = r.Trigger
			event.RoundID = r.RoundID
			if event.ExitIP != "" && (event.Phase == "harvest" || event.Route == "dynamic") {
				r.ExitIP = event.ExitIP
				r.CountryCode = event.CountryCode
			}
		}
		e.eventLocked(event)
	}
}

func detectExitIP(ctx context.Context, client *http.Client, endpoint string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", errors.New("IP check unavailable")
	}
	// No account headers or OAuth token are sent to the IP-check service.
	resp, err := client.Do(req)
	if err != nil {
		return "", errors.New("IP check failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", errors.New("IP check rejected")
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4097))
	if err != nil || len(b) > 4096 {
		return "", errors.New("IP check invalid response")
	}
	var result struct {
		IP string `json:"ip"`
	}
	if json.Unmarshal(b, &result) != nil {
		return "", errors.New("IP check invalid response")
	}
	ip := net.ParseIP(result.IP)
	if ip == nil {
		return "", errors.New("IP check invalid response")
	}
	return ip.String(), nil
}

func (e *Engine) observedProbe(ctx context.Context, c Config, identity *pluginv1.ResolveOutboundIdentityResponse, model, route, state, k string, gen uint64, attempt int, phase string) (string, int, error) {
	return e.observedProbeChecked(ctx, c, identity, model, route, state, k, gen, attempt, phase, false)
}

func (e *Engine) observedProbeChecked(ctx context.Context, c Config, identity *pluginv1.ResolveOutboundIdentityResponse, model, route, state, k string, gen uint64, attempt int, phase string, check bool) (string, int, error) {
	outer := ""
	if phase == "harvest" {
		var err error
		outer, err = e.resolveFrontProxy(ctx, c)
		if err != nil {
			e.activity(k, gen, activityEvent{AccountID: identity.AccountId, Model: model, Attempt: attempt, Phase: phase, Result: "managed_proxy_unavailable", Chained: true})
			return "", 0, err
		}
	} else {
		var err error
		outer, err = e.resolveBusinessFront(ctx, c, route)
		if err != nil {
			e.activity(k, gen, activityEvent{AccountID: identity.AccountId, Model: model, Attempt: attempt, Phase: phase, Result: "managed_proxy_unavailable", Chained: true})
			return "", 0, err
		}
	}
	event := activityEvent{AccountID: identity.AccountId, Model: model, Attempt: attempt, Phase: phase, Result: "started", Chained: outer != ""}
	if phase == "harvest" && check && attempt == 1 {
		if tested := e.testedProxyRoute(c, outer); tested != "" {
			route = tested
			event.ReusedProxySession = true
		}
	}
	e.activity(k, gen, event)
	start := time.Now()
	client, err := harvestClient(route, outer)
	if err != nil {
		event.Result = "transport_unavailable"
		e.activity(k, gen, event)
		return "", 0, err
	}
	defer client.CloseIdleConnections()
	if check {
		e.note(k, gen, attempt, "checking_dynamic_proxy")
		ok, ip, country := e.checkProxyConnectivity(ctx, client, k, gen, identity.AccountId, model, attempt, "dynamic", outer != "")
		event.ExitIP = ip
		event.CountryCode = country
		if !ok {
			e.discardTestedProxy(route)
			event.Result = "dynamic_connectivity_failed"
			event.DurationMS = time.Since(start).Milliseconds()
			e.activity(k, gen, event)
			return "", 0, errConnectivityCheck
		}
		e.note(k, gen, attempt, "")
	}
	if c.ObserveExitIP && !check {
		ip, err := detectExitIP(ctx, client, e.exitIPURL)
		event.ExitIP = ip
		event.CountryCode = e.countryForIP(ctx, ip)
		event.Result = "ip_observed"
		if err != nil {
			event.Result = "ip_check_failed"
		}
		e.activity(k, gen, event)
	}
	detail := probeDetail{}
	result, status, err := e.probeWithClient(ctx, identity, model, state, client, &detail)
	if err != nil && (status == 0 || status == http.StatusForbidden) {
		e.discardTestedProxy(route)
	}
	event.DurationMS = time.Since(start).Milliseconds()
	event.HTTPStatus = status
	event.ActualModel = detail.ActualModel
	event.StateBytes = detail.StateBytes
	event.Result = detail.Result
	if event.Result == "" {
		event.Result = "transport_failed"
	}
	e.activity(k, gen, event)
	return result, status, err
}

type probeDetail struct {
	ActualModel string
	StateBytes  int
	Result      string
}

// Classify errors into fixed, credential-free codes. Never return raw network
// errors: SOCKS and HTTP errors can contain authenticated proxy URLs.
func transportCode(err error) string {
	if errors.Is(err, context.Canceled) {
		return "cancelled"
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "transport_timeout"
	}
	text := strings.ToLower(err.Error())
	if strings.Contains(text, "managed proxy") {
		return "managed_proxy_unavailable"
	}
	if strings.Contains(text, "authentication failed") || strings.Contains(text, "proxy authentication required") {
		return "proxy_auth_failed"
	}
	if strings.Contains(text, "front proxy") {
		return "front_proxy_failed"
	}
	if strings.Contains(text, "certificate") || strings.Contains(text, "tls") {
		return "transport_tls_failed"
	}
	return "transport_failed"
}

// Resolve at every harvest, never cache credentials or silently fall back to direct.
// Validation and ordinary traffic continue through the account's business route.
func (e *Engine) resolveFrontProxy(ctx context.Context, c Config) (string, error) {
	switch frontProxyMode(c) {
	case "direct":
		return "", nil
	case "manual":
		return c.HarvestDialProxyURL, nil
	case "managed":
		e.mu.Lock()
		host := e.host
		e.mu.Unlock()
		if host == nil {
			return "", errManagedProxyUnavailable
		}
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		res, err := host.ResolveProxy(ctx, &pluginv1.ResolveProxyRequest{ProxyId: c.HarvestDialProxyID})
		if err != nil || res == nil || !res.Found || res.ProxyUrl == "" || strings.ContainsAny(res.ProxyUrl, "{}") || validateProxy(res.ProxyUrl) != nil {
			return "", errManagedProxyUnavailable
		}
		return res.ProxyUrl, nil
	default:
		return "", errors.New("invalid front proxy mode")
	}
}

// Manual acquisition checks connectivity before sending any model request.
// IP-check failure is reported as a failed check, not proof that every destination is unreachable.
func (e *Engine) checkProxyConnectivity(ctx context.Context, client *http.Client, k string, gen uint64, id int64, model string, attempt int, route string, chained bool) (bool, string, string) {
	start := time.Now()
	ip, err := detectExitIP(ctx, client, e.exitIPURL)
	country := e.countryForIP(ctx, ip)
	result := "connectivity_ok"
	if err != nil {
		result = route + "_connectivity_failed"
	}
	e.activity(k, gen, activityEvent{AccountID: id, Model: model, Attempt: attempt, Phase: "connectivity", Route: route, CountryCode: country, Result: result, ExitIP: ip, DurationMS: time.Since(start).Milliseconds(), Chained: chained})
	if err != nil {
		e.note(k, gen, attempt, result)
	}
	return err == nil, ip, country
}

// Local proxy bridges already route from this machine; never ask a remote front
// proxy to reach its own loopback. Accounts without a proxy retain direct egress.
func (e *Engine) resolveBusinessFront(ctx context.Context, c Config, route string) (string, error) {
	if !c.BusinessUseFront || route == "" {
		return "", nil
	}
	u, err := url.Parse(route)
	if err != nil {
		return "", errors.New("invalid business proxy")
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "localhost" || net.ParseIP(host).IsLoopback() {
		return "", nil
	}
	return e.resolveFrontProxy(ctx, c)
}

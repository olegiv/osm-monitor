package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// maxBodyBytes caps how much of a response body is read, so a misbehaving
// endpoint cannot exhaust memory.
const maxBodyBytes = 1 << 20

// parseFunc turns an HTTP response into a health verdict. Implementations
// must never include credentials in the returned detail.
type parseFunc func(statusCode int, body []byte) (healthy bool, detail string)

type serviceCheck struct {
	name    string // human-readable, used in alerts ("OSM API")
	key     string // stable state-file key ("osm_api")
	url     string
	headers map[string]string // extra request headers (e.g. ORS Authorization)
	parse   parseFunc
}

type checkResult struct {
	healthy  bool
	detail   string
	attempts int
}

// buildServices returns the enabled checks in a fixed, deterministic order.
func buildServices(cfg *config) []serviceCheck {
	var services []serviceCheck
	if cfg.osmURL != "" {
		services = append(services, serviceCheck{
			name:  "OSM API",
			key:   "osm_api",
			url:   cfg.osmURL,
			parse: parseOSMCapabilities,
		})
	}
	if cfg.nominatimURL != "" {
		services = append(services, serviceCheck{
			name:  "Nominatim",
			key:   "nominatim",
			url:   cfg.nominatimURL,
			parse: parseNominatimStatus,
		})
	}
	if cfg.orsURL != "" {
		services = append(services, serviceCheck{
			name:    "OpenRouteService",
			key:     "ors",
			url:     cfg.orsURL,
			headers: map[string]string{"Authorization": cfg.orsAPIKey},
			parse:   parseORSDirections,
		})
	}
	return services
}

// runCheck probes one service with retries, returning healthy as soon as an
// attempt succeeds. After the final failed attempt the last failure reason is
// kept. Backoff doubles per retry (base, 2×base, ..., capped at maxBackoff);
// sleep is injectable so tests can record delays without waiting.
func runCheck(ctx context.Context, client *http.Client, svc serviceCheck, cfg *config, sleep func(time.Duration)) checkResult {
	var last checkResult
	for attempt := 1; attempt <= cfg.attempts; attempt++ {
		if attempt > 1 {
			sleep(backoffFor(cfg.backoff, attempt))
		}
		healthy, detail := probe(ctx, client, svc, cfg.userAgent)
		last = checkResult{healthy: healthy, detail: sanitizeDetail(detail), attempts: attempt}
		slog.Debug("check attempt", // #nosec G706 -- detail is flattened to one bounded line by sanitizeDetail
			"service", svc.key, "attempt", attempt, "healthy", healthy, "detail", last.detail)
		if healthy || ctx.Err() != nil {
			break
		}
	}
	return last
}

// backoffFor returns the delay before the given 1-based attempt: none before
// the first, then base, 2×base, 4×base, ... capped at maxBackoff. Comparing
// against the right-shifted cap before shifting keeps the doubling from ever
// overflowing time.Duration, independently of config validation (defense in
// depth).
func backoffFor(base time.Duration, attempt int) time.Duration {
	if attempt <= 1 || base <= 0 {
		return 0
	}
	shift := uint(attempt - 2)
	if base > maxBackoff>>shift {
		return maxBackoff
	}
	return base << shift
}

func probe(ctx context.Context, client *http.Client, svc serviceCheck, userAgent string) (bool, string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, svc.url, nil)
	if err != nil {
		return false, fmt.Sprintf("invalid request: %v", err)
	}
	req.Header.Set("User-Agent", userAgent)
	for k, v := range svc.headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req) // #nosec G704 -- service URLs are operator config validated to http(s) at startup; fetching them is this tool's purpose
	if err != nil {
		return false, fmt.Sprintf("request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return false, fmt.Sprintf("reading response: %v", err)
	}
	return svc.parse(resp.StatusCode, body)
}

// parseOSMCapabilities interprets /api/0.6/capabilities.json. The service is
// healthy only when both the API and the database report "online"; gpx is
// informational (not required by consumers of map/editing data).
func parseOSMCapabilities(statusCode int, body []byte) (bool, string) {
	if statusCode != http.StatusOK {
		return false, httpFailureDetail(statusCode, body)
	}
	var payload struct {
		API struct {
			Status struct {
				Database string `json:"database"`
				API      string `json:"api"`
				GPX      string `json:"gpx"`
			} `json:"status"`
		} `json:"api"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return false, "unexpected body: " + excerpt(body)
	}
	st := payload.API.Status
	if st.API == "" && st.Database == "" {
		return false, "unexpected body: missing api.status element"
	}
	detail := fmt.Sprintf("api=%s database=%s gpx=%s", st.API, st.Database, st.GPX)
	return st.API == "online" && st.Database == "online", detail
}

// parseNominatimStatus interprets /status?format=json, which reports
// {"status":0,"message":"OK"} when healthy and a non-zero status code (with
// HTTP 500) when not.
func parseNominatimStatus(statusCode int, body []byte) (bool, string) {
	var payload struct {
		Status      *int   `json:"status"`
		Message     string `json:"message"`
		DataUpdated string `json:"data_updated"`
	}
	parsed := json.Unmarshal(body, &payload) == nil && payload.Status != nil
	if statusCode == http.StatusOK && parsed && *payload.Status == 0 {
		if payload.DataUpdated != "" {
			return true, fmt.Sprintf("%s (data updated %s)", payload.Message, payload.DataUpdated)
		}
		return true, payload.Message
	}
	if parsed {
		return false, fmt.Sprintf("HTTP %d: status=%d message=%s", statusCode, *payload.Status, payload.Message)
	}
	if statusCode != http.StatusOK {
		return false, httpFailureDetail(statusCode, body)
	}
	return false, "unexpected body: " + excerpt(body)
}

// parseORSDirections interprets a minimal directions request. The GET API
// returns a GeoJSON FeatureCollection ("features"); the POST API returns
// "routes" — either non-empty on HTTP 200 counts as healthy, so the check
// proves authentication and the routing engine end-to-end.
func parseORSDirections(statusCode int, body []byte) (bool, string) {
	switch statusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return false, fmt.Sprintf("HTTP %d: authentication failed (check ORS API key)", statusCode)
	case http.StatusTooManyRequests:
		return false, "HTTP 429: rate limited or quota exhausted"
	}
	var payload struct {
		Features []json.RawMessage `json:"features"`
		Routes   []json.RawMessage `json:"routes"`
		Error    json.RawMessage   `json:"error"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		if statusCode != http.StatusOK {
			return false, httpFailureDetail(statusCode, body)
		}
		return false, "unexpected body: " + excerpt(body)
	}
	if results := len(payload.Features) + len(payload.Routes); statusCode == http.StatusOK && results > 0 {
		return true, fmt.Sprintf("routing OK (%d result)", results)
	}
	if len(payload.Error) > 0 {
		return false, fmt.Sprintf("HTTP %d: %s", statusCode, excerpt(payload.Error))
	}
	if statusCode != http.StatusOK {
		return false, httpFailureDetail(statusCode, body)
	}
	return false, "no routes in response"
}

func httpFailureDetail(statusCode int, body []byte) string {
	if s := excerpt(body); s != "" {
		return fmt.Sprintf("HTTP %d: %s", statusCode, s)
	}
	return fmt.Sprintf("HTTP %d", statusCode)
}

// excerpt condenses a response body into a short single-line diagnostic.
func excerpt(body []byte) string {
	const limit = 160
	s := strings.Join(strings.Fields(string(body)), " ")
	runes := []rune(s)
	if len(runes) > limit {
		return string(runes[:limit]) + "…"
	}
	return s
}

// sanitizeDetail flattens remote-derived text into one bounded printable
// line, so response content can never forge log lines or break alert
// markdown. Every checkResult.detail passes through here (see runCheck).
func sanitizeDetail(s string) string {
	clean := strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	clean = strings.Join(strings.Fields(clean), " ")
	const limit = 300
	runes := []rune(clean)
	if len(runes) > limit {
		return string(runes[:limit]) + "…"
	}
	return clean
}

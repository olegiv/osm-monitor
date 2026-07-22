package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const (
	osmHealthyBody  = `{"version":"0.6","generator":"OpenStreetMap server","api":{"status":{"database":"online","api":"online","gpx":"online"}}}`
	nominatimOKBody = `{"status":0,"message":"OK","data_updated":"2026-07-21T11:58:02+00:00"}`
	orsGeoJSONBody  = `{"type":"FeatureCollection","features":[{"type":"Feature","properties":{}}],"metadata":{"engine":{"version":"9.0.0"}}}`
)

func TestParseOSMCapabilities(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		statusCode  int
		body        string
		wantHealthy bool
		wantSub     string
	}{
		{"all online", http.StatusOK, osmHealthyBody, true, "api=online database=online gpx=online"},
		{"database readonly", http.StatusOK,
			`{"api":{"status":{"database":"readonly","api":"online","gpx":"online"}}}`,
			false, "database=readonly"},
		{"api offline", http.StatusOK,
			`{"api":{"status":{"database":"online","api":"offline","gpx":"online"}}}`,
			false, "api=offline"},
		{"gpx offline still healthy", http.StatusOK,
			`{"api":{"status":{"database":"online","api":"online","gpx":"offline"}}}`,
			true, "gpx=offline"},
		{"missing status element", http.StatusOK, `{"version":"0.6","api":{}}`, false, "missing api.status"},
		{"malformed json", http.StatusOK, `<html>oops</html>`, false, "unexpected body"},
		{"http 503 html", http.StatusServiceUnavailable, `<html><body>Maintenance</body></html>`, false, "HTTP 503"},
		{"http 500 empty body", http.StatusInternalServerError, ``, false, "HTTP 500"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			healthy, detail := parseOSMCapabilities(tc.statusCode, []byte(tc.body))
			if healthy != tc.wantHealthy {
				t.Errorf("healthy = %v, want %v (detail %q)", healthy, tc.wantHealthy, detail)
			}
			if !strings.Contains(detail, tc.wantSub) {
				t.Errorf("detail = %q, want substring %q", detail, tc.wantSub)
			}
		})
	}
}

func TestParseNominatimStatus(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		statusCode  int
		body        string
		wantHealthy bool
		wantSub     string
	}{
		{"ok with data timestamp", http.StatusOK, nominatimOKBody, true, "OK (data updated 2026-07-21"},
		{"ok minimal", http.StatusOK, `{"status":0,"message":"OK"}`, true, "OK"},
		{"database error", http.StatusInternalServerError,
			`{"status":700,"message":"Database connection failed"}`,
			false, "status=700 message=Database connection failed"},
		{"error status on http 200", http.StatusOK, `{"status":700,"message":"broken"}`, false, "status=700"},
		{"garbage body", http.StatusOK, `not json at all`, false, "unexpected body"},
		{"empty body", http.StatusOK, ``, false, "unexpected body"},
		{"http 502 html", http.StatusBadGateway, `<html>bad gateway</html>`, false, "HTTP 502"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			healthy, detail := parseNominatimStatus(tc.statusCode, []byte(tc.body))
			if healthy != tc.wantHealthy {
				t.Errorf("healthy = %v, want %v (detail %q)", healthy, tc.wantHealthy, detail)
			}
			if !strings.Contains(detail, tc.wantSub) {
				t.Errorf("detail = %q, want substring %q", detail, tc.wantSub)
			}
		})
	}
}

func TestParseORSDirections(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		statusCode  int
		body        string
		wantHealthy bool
		wantSub     string
	}{
		{"geojson features", http.StatusOK, orsGeoJSONBody, true, "routing OK (1 result)"},
		{"routes shape", http.StatusOK, `{"routes":[{"summary":{"distance":532.5}}]}`, true, "routing OK (1 result)"},
		{"empty features", http.StatusOK, `{"type":"FeatureCollection","features":[]}`, false, "no routes"},
		{"unauthorized", http.StatusUnauthorized, `{"error":"Authorization field missing"}`, false, "authentication failed"},
		{"forbidden", http.StatusForbidden, `{"error":"Access denied"}`, false, "authentication failed"},
		{"rate limited", http.StatusTooManyRequests, `{"error":"Quota exceeded"}`, false, "rate limited or quota exhausted"},
		{"routing error object", http.StatusNotFound,
			`{"error":{"code":2010,"message":"Could not find routable point"}}`,
			false, "2010"},
		{"malformed json", http.StatusOK, `<html>oops</html>`, false, "unexpected body"},
		{"http 503", http.StatusServiceUnavailable, `upstream unavailable`, false, "HTTP 503"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			healthy, detail := parseORSDirections(tc.statusCode, []byte(tc.body))
			if healthy != tc.wantHealthy {
				t.Errorf("healthy = %v, want %v (detail %q)", healthy, tc.wantHealthy, detail)
			}
			if !strings.Contains(detail, tc.wantSub) {
				t.Errorf("detail = %q, want substring %q", detail, tc.wantSub)
			}
		})
	}
}

func testCfg(attempts int, backoff time.Duration) *config {
	return &config{
		attempts:  attempts,
		backoff:   backoff,
		timeout:   time.Second,
		userAgent: "test-agent/1.0",
	}
}

func TestRunCheckRetriesThenSucceeds(t *testing.T) {
	t.Parallel()
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(osmHealthyBody))
	}))
	defer server.Close()

	var sleeps []time.Duration
	svc := serviceCheck{name: "OSM API", key: "osm_api", url: server.URL, parse: parseOSMCapabilities}
	result := runCheck(context.Background(), server.Client(), svc, testCfg(3, 5*time.Second),
		func(d time.Duration) { sleeps = append(sleeps, d) })

	if !result.healthy || result.attempts != 3 {
		t.Errorf("result = %+v, want healthy after 3 attempts", result)
	}
	want := []time.Duration{5 * time.Second, 10 * time.Second}
	if len(sleeps) != len(want) || sleeps[0] != want[0] || sleeps[1] != want[1] {
		t.Errorf("sleeps = %v, want %v (base then doubled)", sleeps, want)
	}
}

func TestBackoffFor(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		base    time.Duration
		attempt int
		want    time.Duration
	}{
		{"no delay before first attempt", 5 * time.Second, 1, 0},
		{"base before second attempt", 5 * time.Second, 2, 5 * time.Second},
		{"doubled before third attempt", 5 * time.Second, 3, 10 * time.Second},
		{"zero base stays zero", 0, 5, 0},
		{"capped instead of multi-day sleep", 5 * time.Second, 20, maxBackoff},
		{"huge shift still capped", 5 * time.Second, 500, maxBackoff},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := backoffFor(tc.base, tc.attempt); got != tc.want {
				t.Errorf("backoffFor(%s, %d) = %s, want %s", tc.base, tc.attempt, got, tc.want)
			}
		})
	}
	// The doubling must never leave [0, maxBackoff] for any attempt count,
	// even with the largest base validation allows (overflow regression net).
	for attempt := 1; attempt <= 1000; attempt++ {
		if d := backoffFor(maxBackoff, attempt); d < 0 || d > maxBackoff {
			t.Fatalf("backoffFor(%s, %d) = %s, want within [0, %s]", maxBackoff, attempt, d, maxBackoff)
		}
	}
}

func TestRunCheckAllAttemptsFail(t *testing.T) {
	t.Parallel()
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("maintenance"))
	}))
	defer server.Close()

	svc := serviceCheck{name: "Nominatim", key: "nominatim", url: server.URL, parse: parseNominatimStatus}
	result := runCheck(context.Background(), server.Client(), svc, testCfg(3, 0),
		func(time.Duration) {})

	if result.healthy {
		t.Error("result.healthy = true, want false")
	}
	if calls != 3 || result.attempts != 3 {
		t.Errorf("calls = %d attempts = %d, want exactly 3 of each", calls, result.attempts)
	}
	if !strings.Contains(result.detail, "HTTP 503") {
		t.Errorf("detail = %q, want last failure reason kept", result.detail)
	}
}

func TestRunCheckFirstTrySuccessSkipsBackoff(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(nominatimOKBody))
	}))
	defer server.Close()

	var sleeps int
	svc := serviceCheck{name: "Nominatim", key: "nominatim", url: server.URL, parse: parseNominatimStatus}
	result := runCheck(context.Background(), server.Client(), svc, testCfg(3, 5*time.Second),
		func(time.Duration) { sleeps++ })

	if !result.healthy || result.attempts != 1 || sleeps != 0 {
		t.Errorf("healthy=%v attempts=%d sleeps=%d, want true/1/0", result.healthy, result.attempts, sleeps)
	}
}

func TestRunCheckTimeout(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(500 * time.Millisecond)
		_, _ = w.Write([]byte(nominatimOKBody))
	}))
	defer server.Close()

	client := &http.Client{Timeout: 50 * time.Millisecond}
	svc := serviceCheck{name: "Nominatim", key: "nominatim", url: server.URL, parse: parseNominatimStatus}
	result := runCheck(context.Background(), client, svc, testCfg(1, 0),
		func(time.Duration) {})

	if result.healthy {
		t.Error("result.healthy = true, want false on timeout")
	}
	if !strings.Contains(result.detail, "request failed") {
		t.Errorf("detail = %q, want request failure", result.detail)
	}
}

func TestRunCheckCanceledContextStopsRetrying(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	svc := serviceCheck{name: "OSM API", key: "osm_api", url: server.URL, parse: parseOSMCapabilities}
	result := runCheck(ctx, server.Client(), svc, testCfg(3, 0), func(time.Duration) {})

	if result.attempts != 1 {
		t.Errorf("attempts = %d, want 1 (no retries after cancellation)", result.attempts)
	}
	if result.healthy {
		t.Error("result.healthy = true, want false")
	}
}

func TestRunCheckSendsHeaders(t *testing.T) {
	t.Parallel()
	const apiKey = "secret-ors-key-12345"
	var gotUA, gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(orsGeoJSONBody))
	}))
	defer server.Close()

	svc := serviceCheck{
		name:    "OpenRouteService",
		key:     "ors",
		url:     server.URL,
		headers: map[string]string{"Authorization": apiKey},
		parse:   parseORSDirections,
	}
	result := runCheck(context.Background(), server.Client(), svc, testCfg(1, 0),
		func(time.Duration) {})

	if gotUA != "test-agent/1.0" {
		t.Errorf("User-Agent = %q, want configured agent (OSM policy)", gotUA)
	}
	if gotAuth != apiKey {
		t.Errorf("Authorization = %q, want the ORS API key", gotAuth)
	}
	if !result.healthy {
		t.Errorf("result = %+v, want healthy", result)
	}
}

func TestCheckerDetailsNeverContainAPIKey(t *testing.T) {
	t.Parallel()
	const apiKey = "secret-ors-key-12345"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"Access denied"}`))
	}))
	defer server.Close()

	svc := serviceCheck{
		name:    "OpenRouteService",
		key:     "ors",
		url:     server.URL,
		headers: map[string]string{"Authorization": apiKey},
		parse:   parseORSDirections,
	}
	result := runCheck(context.Background(), server.Client(), svc, testCfg(2, 0),
		func(time.Duration) {})

	if result.healthy {
		t.Error("result.healthy = true, want false")
	}
	if strings.Contains(result.detail, apiKey) || strings.Contains(result.detail, "secret") {
		t.Errorf("detail %q leaks the API key", result.detail)
	}
}

func TestRunCheckSanitizesDetail(t *testing.T) {
	t.Parallel()
	// Malicious/broken service response: the JSON-decoded message contains a
	// newline (log forgery attempt) and an ANSI escape sequence.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"status":700,"message":"line1\nFAKE log entry\u001b[31mred"}`))
	}))
	defer server.Close()

	svc := serviceCheck{name: "Nominatim", key: "nominatim", url: server.URL, parse: parseNominatimStatus}
	result := runCheck(context.Background(), server.Client(), svc, testCfg(1, 0),
		func(time.Duration) {})

	if result.healthy {
		t.Error("result.healthy = true, want false")
	}
	if strings.ContainsAny(result.detail, "\n\r\x1b") {
		t.Errorf("detail %q contains control characters, want single sanitized line", result.detail)
	}
	if !strings.Contains(result.detail, "line1 FAKE log entry") {
		t.Errorf("detail %q lost its content during sanitizing", result.detail)
	}
}

func TestSanitizeDetailBoundsLength(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", 1000)
	got := sanitizeDetail(long)
	if len([]rune(got)) > 301 {
		t.Errorf("sanitizeDetail left %d runes, want bounded output", len([]rune(got)))
	}
	if sanitizeDetail("plain detail") != "plain detail" {
		t.Error("sanitizeDetail mangled a clean string")
	}
}

func TestBuildServices(t *testing.T) {
	t.Parallel()
	cfg := &config{
		osmURL:       "https://osm.example.org/caps.json",
		nominatimURL: "https://nom.example.org/status",
		orsURL:       "https://ors.example.org/route",
		orsAPIKey:    "k",
	}
	services := buildServices(cfg)
	if len(services) != 3 {
		t.Fatalf("got %d services, want 3", len(services))
	}
	wantKeys := []string{"osm_api", "nominatim", "ors"}
	for i, want := range wantKeys {
		if services[i].key != want {
			t.Errorf("services[%d].key = %q, want %q (deterministic order)", i, services[i].key, want)
		}
	}
	if services[2].headers["Authorization"] != "k" {
		t.Error("ORS service missing Authorization header")
	}

	cfg.nominatimURL = ""
	services = buildServices(cfg)
	if len(services) != 2 {
		t.Fatalf("got %d services after disabling nominatim, want 2", len(services))
	}
	for _, svc := range services {
		if svc.key == "nominatim" {
			t.Error("disabled nominatim check still present")
		}
	}
}

package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	// Keep expected warnings (corrupt state files, failed webhooks) out of
	// the test output; individual tests assert behavior, not log text.
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	os.Exit(m.Run())
}

func staticServer(t *testing.T, statusCode int, body string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(statusCode)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server
}

func recordingWebhook(t *testing.T, statusCode int) (*httptest.Server, *[]string) {
	t.Helper()
	var posts []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		posts = append(posts, string(body))
		w.WriteHeader(statusCode)
	}))
	t.Cleanup(server.Close)
	return server, &posts
}

func e2eDeps(cfg *config, now time.Time, out io.Writer) runDeps {
	if out == nil {
		out = io.Discard
	}
	return runDeps{
		client: &http.Client{Timeout: cfg.timeout},
		sleep:  func(time.Duration) {},
		now:    func() time.Time { return now },
		out:    out,
	}
}

func e2eConfig(t *testing.T, webhookURL, osmURL, nominatimURL string) *config {
	t.Helper()
	return &config{
		webexWebhookURL: webhookURL,
		osmURL:          osmURL,
		nominatimURL:    nominatimURL,
		stateFile:       filepath.Join(t.TempDir(), "state.json"),
		timeout:         2 * time.Second,
		attempts:        1,
		userAgent:       "test-agent/1.0",
	}
}

func TestRunFirstRunDownSendsAlertAndWritesState(t *testing.T) {
	t.Parallel()
	osm := staticServer(t, http.StatusOK, osmHealthyBody)
	nominatim := staticServer(t, http.StatusInternalServerError,
		`{"status":700,"message":"Database connection failed"}`)
	webhook, posts := recordingWebhook(t, http.StatusOK)
	cfg := e2eConfig(t, webhook.URL, osm.URL, nominatim.URL)
	now := time.Date(2026, 7, 21, 12, 5, 0, 0, time.UTC)

	if err := run(context.Background(), cfg, e2eDeps(cfg, now, nil)); err != nil {
		t.Fatalf("run: %v", err)
	}

	if len(*posts) != 1 {
		t.Fatalf("webhook received %d posts, want exactly 1", len(*posts))
	}
	if !strings.Contains((*posts)[0], "DOWN — Nominatim") {
		t.Errorf("post = %q, want a Nominatim DOWN alert", (*posts)[0])
	}
	st := loadState(cfg.stateFile)
	if svc, ok := st.Services["nominatim"]; !ok || svc.Healthy {
		t.Errorf("nominatim state = %+v, want persisted unhealthy", svc)
	}
	if svc, ok := st.Services["osm_api"]; !ok || !svc.Healthy {
		t.Errorf("osm_api state = %+v, want persisted healthy (silent first run)", svc)
	}
}

func TestRunRecoverySendsAlertWithOutageDuration(t *testing.T) {
	t.Parallel()
	nominatim := staticServer(t, http.StatusOK, nominatimOKBody)
	webhook, posts := recordingWebhook(t, http.StatusOK)
	cfg := e2eConfig(t, webhook.URL, "", nominatim.URL)
	now := time.Date(2026, 7, 21, 12, 5, 0, 0, time.UTC)

	prior := newMonitorState()
	prior.Services["nominatim"] = serviceState{
		Healthy:     false,
		Detail:      "HTTP 500: status=700 message=Database connection failed",
		Since:       now.Add(-20 * time.Minute),
		LastChecked: now.Add(-5 * time.Minute),
	}
	if err := saveState(cfg.stateFile, prior); err != nil {
		t.Fatal(err)
	}

	if err := run(context.Background(), cfg, e2eDeps(cfg, now, nil)); err != nil {
		t.Fatalf("run: %v", err)
	}

	if len(*posts) != 1 {
		t.Fatalf("webhook received %d posts, want exactly 1", len(*posts))
	}
	for _, want := range []string{"RECOVERED — Nominatim", "outage duration: 20m0s"} {
		if !strings.Contains((*posts)[0], want) {
			t.Errorf("post = %q, want substring %q", (*posts)[0], want)
		}
	}
	if svc := loadState(cfg.stateFile).Services["nominatim"]; !svc.Healthy {
		t.Errorf("nominatim state = %+v, want healthy after recovery", svc)
	}
}

func TestRunWebhookFailureKeepsStateForRetry(t *testing.T) {
	t.Parallel()
	osm := staticServer(t, http.StatusServiceUnavailable, "maintenance")
	nominatim := staticServer(t, http.StatusOK, nominatimOKBody)
	webhook, _ := recordingWebhook(t, http.StatusInternalServerError)
	cfg := e2eConfig(t, webhook.URL, osm.URL, nominatim.URL)
	now := time.Date(2026, 7, 21, 12, 5, 0, 0, time.UTC)

	err := run(context.Background(), cfg, e2eDeps(cfg, now, nil))
	if !errors.Is(err, errNotify) {
		t.Fatalf("run error = %v, want errNotify (exit code 3)", err)
	}

	st := loadState(cfg.stateFile)
	if _, ok := st.Services["osm_api"]; ok {
		t.Error("osm_api state was committed despite failed alert; next run would never retry")
	}
	if svc, ok := st.Services["nominatim"]; !ok || !svc.Healthy {
		t.Errorf("nominatim state = %+v, want committed healthy entry", svc)
	}
}

func TestRunSteadyStateSendsNothing(t *testing.T) {
	t.Parallel()
	osm := staticServer(t, http.StatusOK, osmHealthyBody)
	nominatim := staticServer(t, http.StatusOK, nominatimOKBody)
	webhook, posts := recordingWebhook(t, http.StatusOK)
	cfg := e2eConfig(t, webhook.URL, osm.URL, nominatim.URL)
	now := time.Date(2026, 7, 21, 12, 5, 0, 0, time.UTC)
	since := now.Add(-2 * time.Hour)

	prior := newMonitorState()
	prior.Services["osm_api"] = serviceState{Healthy: true, Since: since, LastChecked: since}
	prior.Services["nominatim"] = serviceState{Healthy: true, Since: since, LastChecked: since}
	if err := saveState(cfg.stateFile, prior); err != nil {
		t.Fatal(err)
	}

	if err := run(context.Background(), cfg, e2eDeps(cfg, now, nil)); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(*posts) != 0 {
		t.Errorf("webhook received %d posts, want 0 in steady state", len(*posts))
	}
	st := loadState(cfg.stateFile)
	if svc := st.Services["osm_api"]; !svc.Since.Equal(since) || !svc.LastChecked.Equal(now) {
		t.Errorf("osm_api = %+v, want since kept (%s) and last_checked advanced (%s)", svc, since, now)
	}
}

func TestRunDryRunSendsAndWritesNothing(t *testing.T) {
	t.Parallel()
	osm := staticServer(t, http.StatusServiceUnavailable, "maintenance")
	webhook, posts := recordingWebhook(t, http.StatusOK)
	cfg := e2eConfig(t, webhook.URL, osm.URL, "")
	cfg.dryRun = true
	now := time.Date(2026, 7, 21, 12, 5, 0, 0, time.UTC)
	var out bytes.Buffer

	if err := run(context.Background(), cfg, e2eDeps(cfg, now, &out)); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(*posts) != 0 {
		t.Errorf("webhook received %d posts in dry-run, want 0", len(*posts))
	}
	for _, want := range []string{"[dry-run]", "DOWN — OSM API"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("dry-run output %q missing %q", out.String(), want)
		}
	}
	if _, err := os.Stat(cfg.stateFile); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("state file exists after dry-run (stat err = %v), want no write", err)
	}
}

func TestRunHeartbeatSendsSummaryInSteadyState(t *testing.T) {
	t.Parallel()
	osm := staticServer(t, http.StatusOK, osmHealthyBody)
	nominatim := staticServer(t, http.StatusOK, nominatimOKBody)
	webhook, posts := recordingWebhook(t, http.StatusOK)
	cfg := e2eConfig(t, webhook.URL, osm.URL, nominatim.URL)
	cfg.heartbeat = true
	now := time.Date(2026, 7, 22, 9, 2, 0, 0, time.UTC)
	since := now.Add(-2 * time.Hour)

	prior := newMonitorState()
	prior.Services["osm_api"] = serviceState{Healthy: true, Since: since, LastChecked: since}
	prior.Services["nominatim"] = serviceState{Healthy: true, Since: since, LastChecked: since}
	if err := saveState(cfg.stateFile, prior); err != nil {
		t.Fatal(err)
	}

	if err := run(context.Background(), cfg, e2eDeps(cfg, now, nil)); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(*posts) != 1 {
		t.Fatalf("webhook received %d posts, want exactly 1 (heartbeat only, no alerts)", len(*posts))
	}
	for _, want := range []string{"HEARTBEAT", "OSM API ✅", "Nominatim ✅"} {
		if !strings.Contains((*posts)[0], want) {
			t.Errorf("post = %q, want substring %q", (*posts)[0], want)
		}
	}
}

func TestRunHeartbeatAlongsideTransitionAlert(t *testing.T) {
	t.Parallel()
	osm := staticServer(t, http.StatusServiceUnavailable, "maintenance")
	nominatim := staticServer(t, http.StatusOK, nominatimOKBody)
	webhook, posts := recordingWebhook(t, http.StatusOK)
	cfg := e2eConfig(t, webhook.URL, osm.URL, nominatim.URL)
	cfg.heartbeat = true
	now := time.Date(2026, 7, 22, 9, 2, 0, 0, time.UTC)

	if err := run(context.Background(), cfg, e2eDeps(cfg, now, nil)); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(*posts) != 2 {
		t.Fatalf("webhook received %d posts, want 2 (DOWN alert + heartbeat)", len(*posts))
	}
	if !strings.Contains((*posts)[0], "DOWN — OSM API") {
		t.Errorf("first post = %q, want the DOWN alert", (*posts)[0])
	}
	for _, want := range []string{"HEARTBEAT", "OSM API ❌", "Nominatim ✅"} {
		if !strings.Contains((*posts)[1], want) {
			t.Errorf("second post = %q, want heartbeat with %q", (*posts)[1], want)
		}
	}
}

func TestRunHeartbeatFailureExitsNotifyButCommitsState(t *testing.T) {
	t.Parallel()
	osm := staticServer(t, http.StatusOK, osmHealthyBody)
	webhook, _ := recordingWebhook(t, http.StatusInternalServerError)
	cfg := e2eConfig(t, webhook.URL, osm.URL, "")
	cfg.heartbeat = true
	now := time.Date(2026, 7, 22, 9, 2, 0, 0, time.UTC)

	err := run(context.Background(), cfg, e2eDeps(cfg, now, nil))
	if !errors.Is(err, errNotify) {
		t.Fatalf("run error = %v, want errNotify (exit code 3)", err)
	}
	// Unlike a failed transition alert, a failed heartbeat must not block
	// the state commit: the cycle itself succeeded.
	if svc, ok := loadState(cfg.stateFile).Services["osm_api"]; !ok || !svc.Healthy {
		t.Errorf("osm_api state = %+v, want committed healthy entry despite heartbeat failure", svc)
	}
}

func TestRunDryRunHeartbeatPrintsInsteadOfSending(t *testing.T) {
	t.Parallel()
	osm := staticServer(t, http.StatusOK, osmHealthyBody)
	webhook, posts := recordingWebhook(t, http.StatusOK)
	cfg := e2eConfig(t, webhook.URL, osm.URL, "")
	cfg.heartbeat = true
	cfg.dryRun = true
	var out bytes.Buffer

	if err := run(context.Background(), cfg, e2eDeps(cfg, time.Now(), &out)); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(*posts) != 0 {
		t.Errorf("webhook received %d posts in dry-run, want 0", len(*posts))
	}
	for _, want := range []string{"[dry-run] would send Webex heartbeat", "HEARTBEAT", "OSM API ✅"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("dry-run output %q missing %q", out.String(), want)
		}
	}
	if _, err := os.Stat(cfg.stateFile); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("state file exists after dry-run (stat err = %v), want no write", err)
	}
}

func TestRunStatePersistFailure(t *testing.T) {
	t.Parallel()
	osm := staticServer(t, http.StatusOK, osmHealthyBody)
	webhook, _ := recordingWebhook(t, http.StatusOK)
	cfg := e2eConfig(t, webhook.URL, osm.URL, "")
	cfg.stateFile = filepath.Join(t.TempDir(), "missing-dir", "state.json")

	err := run(context.Background(), cfg, e2eDeps(cfg, time.Now(), nil))
	if !errors.Is(err, errState) {
		t.Fatalf("run error = %v, want errState (exit code 4)", err)
	}
}

func TestWantsVersion(t *testing.T) {
	t.Parallel()
	cases := []struct {
		args []string
		want bool
	}{
		{[]string{"-v"}, true},
		{[]string{"-version"}, true},
		{[]string{"--version"}, true},
		{[]string{"--verbose"}, false},
		{[]string{}, false},
		{[]string{"--dry-run", "--version"}, true},
	}
	for _, tc := range cases {
		if got := wantsVersion(tc.args); got != tc.want {
			t.Errorf("wantsVersion(%v) = %v, want %v", tc.args, got, tc.want)
		}
	}
}

func TestPrintVersion(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	printVersion(&buf)
	out := buf.String()
	for _, want := range []string{"osm-monitor", version, gitCommit, "go1."} {
		if !strings.Contains(out, want) {
			t.Errorf("version output %q missing %q", out, want)
		}
	}
}

func TestPrintUsage(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	printUsage(&buf)
	out := buf.String()
	for _, want := range []string{
		"--webex-webhook-url", "OSMMON_WEBEX_WEBHOOK_URL",
		"--ors-api-key", "ORS_API_KEY",
		"--env-file", "--dry-run", "--state-file",
		"Exit codes", "--version",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("usage output missing %q", want)
		}
	}
}

func TestStatusWord(t *testing.T) {
	t.Parallel()
	if statusWord(true) != "up" || statusWord(false) != "down" {
		t.Error(`statusWord mapping wrong, want true->"up" false->"down"`)
	}
}

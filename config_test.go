package main

import (
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func lookupFrom(m map[string]string) envLookup {
	return func(key string) (string, bool) {
		v, ok := m[key]
		return v, ok
	}
}

func writeTempEnvFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.env")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestParseConfigDefaults(t *testing.T) {
	t.Parallel()
	cfg, err := parseConfig([]string{"--dry-run"}, lookupFrom(map[string]string{"ORS_API_KEY": "key1"}))
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if cfg.osmURL != defaultOSMURL {
		t.Errorf("osmURL = %q, want %q", cfg.osmURL, defaultOSMURL)
	}
	if cfg.nominatimURL != defaultNominatimURL {
		t.Errorf("nominatimURL = %q, want %q", cfg.nominatimURL, defaultNominatimURL)
	}
	if cfg.orsURL != defaultORSURL {
		t.Errorf("orsURL = %q, want %q", cfg.orsURL, defaultORSURL)
	}
	if cfg.orsAPIKey != "key1" {
		t.Errorf("orsAPIKey = %q, want fallback ORS_API_KEY value", cfg.orsAPIKey)
	}
	if cfg.stateFile != defaultStateFile {
		t.Errorf("stateFile = %q, want %q", cfg.stateFile, defaultStateFile)
	}
	if cfg.timeout != defaultTimeout || cfg.attempts != defaultAttempts || cfg.backoff != defaultBackoff {
		t.Errorf("timing defaults = (%s, %d, %s), want (%s, %d, %s)",
			cfg.timeout, cfg.attempts, cfg.backoff, defaultTimeout, defaultAttempts, defaultBackoff)
	}
	if !strings.Contains(cfg.userAgent, "iru-osm-monitor/") {
		t.Errorf("userAgent = %q, want default containing iru-osm-monitor/", cfg.userAgent)
	}
	if cfg.verbose || !cfg.dryRun {
		t.Errorf("verbose = %v dryRun = %v, want false true", cfg.verbose, cfg.dryRun)
	}
	if cfg.heartbeat {
		t.Error("heartbeat = true, want false by default")
	}
}

func TestParseConfigHeartbeatFlag(t *testing.T) {
	t.Parallel()
	cfg, err := parseConfig([]string{"--heartbeat", "--dry-run"},
		lookupFrom(map[string]string{"ORS_API_KEY": "k"}))
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if !cfg.heartbeat {
		t.Error("heartbeat = false, want true from --heartbeat")
	}
}

func TestParseConfigEnvOverrides(t *testing.T) {
	t.Parallel()
	env := map[string]string{
		"OSMMON_WEBEX_WEBHOOK_URL": "https://webexapis.com/v1/webhooks/incoming/abc",
		"OSMMON_OSM_URL":           "https://osm.example.org/caps.json",
		"OSMMON_NOMINATIM_URL":     "https://nom.example.org/status",
		"OSMMON_ORS_URL":           "https://ors.example.org/route",
		"OSMMON_ORS_API_KEY":       "primary-key",
		"ORS_API_KEY":              "fallback-key",
		"OSMMON_STATE_FILE":        "/var/lib/osm-monitor/state.json",
		"OSMMON_TIMEOUT":           "3s",
		"OSMMON_ATTEMPTS":          "5",
		"OSMMON_BACKOFF":           "2s",
		"OSMMON_USER_AGENT":        "custom-agent/1.0",
		"OSMMON_VERBOSE":           "1",
	}
	cfg, err := parseConfig(nil, lookupFrom(env))
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if cfg.webexWebhookURL != env["OSMMON_WEBEX_WEBHOOK_URL"] {
		t.Errorf("webexWebhookURL = %q", cfg.webexWebhookURL)
	}
	if cfg.orsAPIKey != "primary-key" {
		t.Errorf("orsAPIKey = %q, want OSMMON_ORS_API_KEY to win over ORS_API_KEY", cfg.orsAPIKey)
	}
	if cfg.stateFile != "/var/lib/osm-monitor/state.json" {
		t.Errorf("stateFile = %q", cfg.stateFile)
	}
	if cfg.timeout != 3*time.Second || cfg.attempts != 5 || cfg.backoff != 2*time.Second {
		t.Errorf("timing = (%s, %d, %s)", cfg.timeout, cfg.attempts, cfg.backoff)
	}
	if cfg.userAgent != "custom-agent/1.0" {
		t.Errorf("userAgent = %q", cfg.userAgent)
	}
	if !cfg.verbose {
		t.Error("verbose = false, want true from OSMMON_VERBOSE=1")
	}
}

func TestParseConfigFlagsBeatEnv(t *testing.T) {
	t.Parallel()
	env := map[string]string{
		"OSMMON_WEBEX_WEBHOOK_URL": "https://env.example.org/hook",
		"OSMMON_TIMEOUT":           "3s",
		"OSMMON_ORS_URL":           "", // env empty string disables ORS
	}
	args := []string{
		"--webex-webhook-url", "https://flag.example.org/hook",
		"--timeout", "7s",
	}
	cfg, err := parseConfig(args, lookupFrom(env))
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if cfg.webexWebhookURL != "https://flag.example.org/hook" {
		t.Errorf("webexWebhookURL = %q, want flag value", cfg.webexWebhookURL)
	}
	if cfg.timeout != 7*time.Second {
		t.Errorf("timeout = %s, want 7s from flag", cfg.timeout)
	}
	if cfg.orsURL != "" {
		t.Errorf("orsURL = %q, want disabled by empty env value", cfg.orsURL)
	}
}

func TestParseConfigEnvFilePrecedence(t *testing.T) {
	t.Parallel()
	envFile := writeTempEnvFile(t, strings.Join([]string{
		`OSMMON_WEBEX_WEBHOOK_URL="https://file.example.org/hook"`,
		`OSMMON_TIMEOUT="9s"`,
		`OSMMON_ATTEMPTS="4"`,
		`ORS_API_KEY="file-key"`,
	}, "\n"))
	realEnv := map[string]string{"OSMMON_TIMEOUT": "2s"}
	args := []string{"--env-file", envFile, "--attempts", "6"}
	cfg, err := parseConfig(args, lookupFrom(realEnv))
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if cfg.webexWebhookURL != "https://file.example.org/hook" {
		t.Errorf("webexWebhookURL = %q, want env-file value", cfg.webexWebhookURL)
	}
	if cfg.timeout != 2*time.Second {
		t.Errorf("timeout = %s, want real env (2s) to beat env-file (9s)", cfg.timeout)
	}
	if cfg.attempts != 6 {
		t.Errorf("attempts = %d, want flag (6) to beat env-file (4)", cfg.attempts)
	}
	if cfg.orsAPIKey != "file-key" {
		t.Errorf("orsAPIKey = %q, want env-file fallback key", cfg.orsAPIKey)
	}
}

func TestParseConfigEnvFileEqualsSyntax(t *testing.T) {
	t.Parallel()
	envFile := writeTempEnvFile(t, `OSMMON_USER_AGENT="equals-agent/1.0"`)
	cfg, err := parseConfig([]string{"--env-file=" + envFile, "--dry-run", "--ors-url", ""},
		lookupFrom(nil))
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if cfg.userAgent != "equals-agent/1.0" {
		t.Errorf("userAgent = %q, want value from --env-file=path syntax", cfg.userAgent)
	}
}

func TestParseConfigErrors(t *testing.T) {
	t.Parallel()
	base := map[string]string{
		"OSMMON_WEBEX_WEBHOOK_URL": "https://webexapis.com/v1/webhooks/incoming/abc",
		"ORS_API_KEY":              "key1",
	}
	withBase := func(extra map[string]string) map[string]string {
		m := make(map[string]string, len(base)+len(extra))
		maps.Copy(m, base)
		maps.Copy(m, extra)
		return m
	}

	cases := []struct {
		name    string
		args    []string
		env     map[string]string
		wantSub string
	}{
		{"missing webhook", nil, map[string]string{"ORS_API_KEY": "k"}, "webhook URL is required"},
		{"bad timeout env", nil, withBase(map[string]string{"OSMMON_TIMEOUT": "abc"}), "invalid duration"},
		{"bad attempts env", nil, withBase(map[string]string{"OSMMON_ATTEMPTS": "many"}), "invalid integer"},
		{"bad verbose env", nil, withBase(map[string]string{"OSMMON_VERBOSE": "maybe"}), "invalid boolean"},
		{"zero attempts flag", []string{"--attempts", "0"}, base, "attempts must be between 1 and"},
		{"excessive attempts flag", []string{"--attempts", "20"}, base, "attempts must be between 1 and"},
		{"zero timeout flag", []string{"--timeout", "0s"}, base, "timeout must be > 0"},
		{"negative backoff flag", []string{"--backoff", "-1s"}, base, "backoff must be between 0 and"},
		{"excessive backoff flag", []string{"--backoff", "10m"}, base, "backoff must be between 0 and"},
		{"empty user agent", []string{"--user-agent", "  "}, base, "user agent"},
		{"non-http url", []string{"--osm-url", "ftp://osm.example.org"}, base, "invalid URL"},
		{"garbage webhook url", nil, map[string]string{
			"OSMMON_WEBEX_WEBHOOK_URL": "not a url", "ORS_API_KEY": "k"}, "invalid URL"},
		{"http webhook url", nil, map[string]string{
			"OSMMON_WEBEX_WEBHOOK_URL": "http://webexapis.com/v1/webhooks/incoming/abc",
			"ORS_API_KEY":              "k"}, "must use https"},
		{"http ors url", []string{"--ors-url", "http://ors.example.org/route"}, base, "must use https"},
		{"missing ors key", nil, map[string]string{
			"OSMMON_WEBEX_WEBHOOK_URL": "https://webexapis.com/v1/webhooks/incoming/abc"},
			"ORS API key is required"},
		{"all checks disabled", []string{"--osm-url", "", "--nominatim-url", "", "--ors-url", ""},
			base, "nothing to monitor"},
		{"unknown flag", []string{"--bogus"}, base, "not defined"},
		{"positional args", []string{"stray"}, base, "unexpected arguments"},
		{"env-file missing value", []string{"--env-file"}, base, "requires a value"},
		{"env-file not found", []string{"--env-file", "/nonexistent/path.env"}, base, "env file"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := parseConfig(tc.args, lookupFrom(tc.env))
			if err == nil {
				t.Fatal("parseConfig succeeded, want error")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error = %q, want substring %q", err, tc.wantSub)
			}
		})
	}
}

func TestParseConfigAllowsHTTPForCredentialFreeURLs(t *testing.T) {
	t.Parallel()
	cfg, err := parseConfig(
		[]string{"--osm-url", "http://internal.example.org/caps.json", "--dry-run"},
		lookupFrom(map[string]string{"ORS_API_KEY": "k"}))
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if cfg.osmURL != "http://internal.example.org/caps.json" {
		t.Errorf("osmURL = %q, want plain-http URL accepted for credential-free check", cfg.osmURL)
	}
}

func TestParseConfigDoesNotExposeCredentialURLsInErrors(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		args   []string
		env    map[string]string
		secret string
	}{
		{
			name: "http webhook",
			env: map[string]string{
				"OSMMON_WEBEX_WEBHOOK_URL": "http://webex.example.test/incoming/webhook-secret",
				"ORS_API_KEY":              "key",
			},
			secret: "webhook-secret",
		},
		{
			name:   "invalid ORS URL",
			args:   []string{"--ors-url", "not-a-url-with-ors-secret"},
			env:    map[string]string{"OSMMON_WEBEX_WEBHOOK_URL": "https://webex.example.test/hook", "ORS_API_KEY": "key"},
			secret: "ors-secret",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := parseConfig(tc.args, lookupFrom(tc.env))
			if err == nil {
				t.Fatal("parseConfig succeeded, want error")
			}
			if strings.Contains(err.Error(), tc.secret) {
				t.Errorf("error exposes credential URL: %q", err)
			}
			if strings.Contains(err.Error(), "http or https") {
				t.Errorf("error suggests plain http is acceptable for a credential URL: %q", err)
			}
			if !strings.Contains(err.Error(), "https") {
				t.Errorf("error does not point to the https requirement: %q", err)
			}
		})
	}
}

func TestParseEnvFile(t *testing.T) {
	t.Parallel()
	content := strings.Join([]string{
		"# leading comment",
		"",
		`OSMMON_TIMEOUT="10s"`,
		`OSMMON_BACKOFF='5s'`,
		"OSMMON_ATTEMPTS=3",
		"export OSMMON_VERBOSE=1",
		"  OSMMON_STATE_FILE = ./state.json  ",
	}, "\n")
	pairs, err := parseEnvFile(writeTempEnvFile(t, content))
	if err != nil {
		t.Fatalf("parseEnvFile: %v", err)
	}
	want := map[string]string{
		"OSMMON_TIMEOUT":    "10s",
		"OSMMON_BACKOFF":    "5s",
		"OSMMON_ATTEMPTS":   "3",
		"OSMMON_VERBOSE":    "1",
		"OSMMON_STATE_FILE": "./state.json",
	}
	if len(pairs) != len(want) {
		t.Fatalf("got %d pairs, want %d", len(pairs), len(want))
	}
	for _, p := range pairs {
		if want[p.key] != p.value {
			t.Errorf("%s = %q, want %q", p.key, p.value, want[p.key])
		}
	}
}

func TestParseEnvFileMalformed(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		content string
		wantSub string
	}{
		{"no equals", "JUST_A_WORD", "expected KEY=value"},
		{"invalid key", "9BAD=value", "invalid key"},
		{"key with space", "BAD KEY=value", "invalid key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := parseEnvFile(writeTempEnvFile(t, tc.content))
			if err == nil {
				t.Fatal("parseEnvFile succeeded, want error")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error = %q, want substring %q", err, tc.wantSub)
			}
		})
	}
}

func TestDefaultUserAgentEmbedsVersion(t *testing.T) {
	ua := defaultUserAgent()
	if !strings.Contains(ua, version) {
		t.Errorf("defaultUserAgent() = %q, want it to contain version %q", ua, version)
	}
	if !strings.Contains(ua, "@") {
		t.Errorf("defaultUserAgent() = %q, want contact info per OSM usage policy", ua)
	}
}

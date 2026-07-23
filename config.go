package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// envLookup abstracts os.LookupEnv so tests can inject fixed environments.
type envLookup func(key string) (string, bool)

// config holds all runtime settings, resolved with precedence:
// flags > process env > --env-file entries > built-in defaults.
type config struct {
	webexWebhookURL string
	osmURL          string
	nominatimURL    string
	orsURL          string
	orsAPIKey       string
	stateFile       string
	timeout         time.Duration
	attempts        int
	backoff         time.Duration
	userAgent       string
	verbose         bool
	dryRun          bool
	heartbeat       bool
}

const (
	defaultOSMURL       = "https://api.openstreetmap.org/api/0.6/capabilities.json"
	defaultNominatimURL = "https://nominatim.openstreetmap.org/status?format=json"
	defaultORSURL       = "https://api.openrouteservice.org/v2/directions/driving-car?start=8.681495,49.41461&end=8.687872,49.420318"
	defaultStateFile    = "./osm-monitor-state.json"
	defaultTimeout      = 10 * time.Second
	defaultAttempts     = 3
	defaultBackoff      = 5 * time.Second

	// maxAttempts and maxBackoff bound the retry knobs: the backoff doubles
	// per attempt, so an unbounded value (e.g. a typo like --attempts 20)
	// would turn one retry delay into days — and under a flock-based cron
	// setup a stuck cycle blocks every later run. See backoffFor in
	// checker.go for the matching runtime clamp.
	maxAttempts = 10
	maxBackoff  = 5 * time.Minute
)

// defaultUserAgent identifies this monitor per the OSM/Nominatim usage
// policy, which requires a User-Agent with contact information.
func defaultUserAgent() string {
	return fmt.Sprintf("iru-osm-monitor/%s (+oleg.a.ivanchenko@gmail.com)", version)
}

func parseConfig(args []string, lookup envLookup) (*config, error) {
	merged, err := mergedLookup(args, lookup)
	if err != nil {
		return nil, err
	}

	strVal := func(key, def string) string {
		if v, ok := merged(key); ok {
			return v
		}
		return def
	}
	durVal := func(key string, def time.Duration) (time.Duration, error) {
		v, ok := merged(key)
		if !ok {
			return def, nil
		}
		d, err := time.ParseDuration(v)
		if err != nil {
			return 0, fmt.Errorf("%s: invalid duration %q", key, v)
		}
		return d, nil
	}

	timeoutDef, err := durVal("OSMMON_TIMEOUT", defaultTimeout)
	if err != nil {
		return nil, err
	}
	backoffDef, err := durVal("OSMMON_BACKOFF", defaultBackoff)
	if err != nil {
		return nil, err
	}
	attemptsDef := defaultAttempts
	if v, ok := merged("OSMMON_ATTEMPTS"); ok {
		attemptsDef, err = strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("OSMMON_ATTEMPTS: invalid integer %q", v)
		}
	}
	verboseDef := false
	if v, ok := merged("OSMMON_VERBOSE"); ok {
		verboseDef, err = strconv.ParseBool(v)
		if err != nil {
			return nil, fmt.Errorf("OSMMON_VERBOSE: invalid boolean %q", v)
		}
	}
	orsKeyDef := strVal("OSMMON_ORS_API_KEY", "")
	if orsKeyDef == "" {
		orsKeyDef = strVal("ORS_API_KEY", "")
	}

	cfg := &config{}
	fs := flag.NewFlagSet("osm-monitor", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&cfg.webexWebhookURL, "webex-webhook-url", strVal("OSMMON_WEBEX_WEBHOOK_URL", ""),
		"Webex incoming webhook URL (required unless --dry-run)")
	fs.StringVar(&cfg.osmURL, "osm-url", strVal("OSMMON_OSM_URL", defaultOSMURL),
		"OSM API capabilities URL (empty disables the check)")
	fs.StringVar(&cfg.nominatimURL, "nominatim-url", strVal("OSMMON_NOMINATIM_URL", defaultNominatimURL),
		"Nominatim status URL (empty disables the check)")
	fs.StringVar(&cfg.orsURL, "ors-url", strVal("OSMMON_ORS_URL", defaultORSURL),
		"OpenRouteService directions URL (empty disables the check)")
	fs.StringVar(&cfg.orsAPIKey, "ors-api-key", orsKeyDef,
		"OpenRouteService API key (required while the ORS check is enabled)")
	fs.StringVar(&cfg.stateFile, "state-file", strVal("OSMMON_STATE_FILE", defaultStateFile),
		"Path to the JSON state file")
	fs.DurationVar(&cfg.timeout, "timeout", timeoutDef, "HTTP timeout per request")
	fs.IntVar(&cfg.attempts, "attempts", attemptsDef, "Attempts per service before declaring DOWN")
	fs.DurationVar(&cfg.backoff, "backoff", backoffDef, "Base backoff between attempts (doubles per retry)")
	fs.StringVar(&cfg.userAgent, "user-agent", strVal("OSMMON_USER_AGENT", defaultUserAgent()),
		"User-Agent header for check requests")
	fs.BoolVar(&cfg.verbose, "verbose", verboseDef, "Enable debug logging")
	fs.BoolVar(&cfg.dryRun, "dry-run", false,
		"Check services and print transitions without sending alerts or saving state")
	fs.BoolVar(&cfg.heartbeat, "heartbeat", false,
		"Also post a heartbeat summary message for this cycle")
	// Consumed by the pre-scan in mergedLookup; registered so Parse accepts it.
	fs.String("env-file", "", "Load environment defaults from a shell-compatible KEY=value file")

	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if rest := fs.Args(); len(rest) > 0 {
		return nil, fmt.Errorf("unexpected arguments: %s", strings.Join(rest, " "))
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *config) validate() error {
	if c.attempts < 1 || c.attempts > maxAttempts {
		return fmt.Errorf("attempts must be between 1 and %d, got %d", maxAttempts, c.attempts)
	}
	if c.timeout <= 0 {
		return fmt.Errorf("timeout must be > 0, got %s", c.timeout)
	}
	if c.backoff < 0 || c.backoff > maxBackoff {
		return fmt.Errorf("backoff must be between 0 and %s, got %s", maxBackoff, c.backoff)
	}
	if strings.TrimSpace(c.userAgent) == "" {
		return errors.New("user agent must not be empty")
	}
	if !c.dryRun && c.webexWebhookURL == "" {
		return errors.New("webex webhook URL is required (set OSMMON_WEBEX_WEBHOOK_URL or --webex-webhook-url)")
	}
	for _, item := range []struct {
		name, value string
		httpsOnly   bool
	}{
		{name: "webex-webhook-url", value: c.webexWebhookURL, httpsOnly: true},
		{name: "osm-url", value: c.osmURL},
		{name: "nominatim-url", value: c.nominatimURL},
		{name: "ors-url", value: c.orsURL, httpsOnly: true},
	} {
		if item.value == "" {
			continue
		}
		if err := validateHTTPURL(item.name, item.value, item.httpsOnly); err != nil {
			return err
		}
	}
	if c.orsURL != "" && c.orsAPIKey == "" {
		return errors.New("ORS API key is required while the ORS check is enabled " +
			`(set OSMMON_ORS_API_KEY or ORS_API_KEY, or disable with --ors-url "")`)
	}
	if c.osmURL == "" && c.nominatimURL == "" && c.orsURL == "" {
		return errors.New("all service checks are disabled; nothing to monitor")
	}
	return nil
}

// validateHTTPURL checks that raw is a usable http(s) URL. httpsOnly is set
// for credential-bearing URLs — the Webex webhook URL is itself a bearer
// token and ORS requests carry the API key — which must never cross the
// network in cleartext.
func validateHTTPURL(name, raw string, httpsOnly bool) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		if httpsOnly {
			return fmt.Errorf("%s: invalid URL (must be http or https)", name)
		}
		return fmt.Errorf("%s: invalid URL %q (must be http or https)", name, raw)
	}
	if httpsOnly && u.Scheme != "https" {
		return fmt.Errorf("%s: must use https (the request carries a credential)", name)
	}
	return nil
}

// mergedLookup layers --env-file entries (when the flag is present in args)
// under the process environment: real env vars win over file entries.
func mergedLookup(args []string, lookup envLookup) (envLookup, error) {
	path, err := envFileArg(args)
	if err != nil {
		return nil, err
	}
	if path == "" {
		return lookup, nil
	}
	pairs, err := parseEnvFile(path)
	if err != nil {
		return nil, err
	}
	fromFile := make(map[string]string, len(pairs))
	for _, p := range pairs {
		fromFile[p.key] = p.value
	}
	return func(key string) (string, bool) {
		if v, ok := lookup(key); ok {
			return v, true
		}
		v, ok := fromFile[key]
		return v, ok
	}, nil
}

// envFileArg pre-scans raw args for --env-file so the file's entries can act
// as defaults during flag registration, keeping precedence flags > env >
// env-file > defaults in a single Parse pass.
func envFileArg(args []string) (string, error) {
	for i := range len(args) {
		arg := args[i]
		if arg == "--" {
			break
		}
		if !strings.HasPrefix(arg, "-") {
			continue
		}
		body := strings.TrimLeft(arg, "-")
		switch {
		case body == "env-file":
			if i+1 >= len(args) {
				return "", errors.New("flag --env-file requires a value")
			}
			return args[i+1], nil
		case strings.HasPrefix(body, "env-file="):
			return strings.TrimPrefix(body, "env-file="), nil
		}
	}
	return "", nil
}

type envPair struct {
	key   string
	value string
}

// parseEnvFile reads a shell-compatible KEY=value file (same format as
// .env.example): blank lines and #-comments are skipped, an optional
// "export " prefix is allowed, and values may be wrapped in single or double
// quotes. Escape sequences and multi-line values are not supported.
func parseEnvFile(path string) ([]envPair, error) {
	data, err := os.ReadFile(filepath.Clean(path)) // #nosec G304 G703 -- operator-supplied config path
	if err != nil {
		return nil, fmt.Errorf("env file: %w", err)
	}
	var pairs []envPair
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("env file %s:%d: expected KEY=value, got %q", path, i+1, line)
		}
		key = strings.TrimSpace(key)
		if !validEnvKey(key) {
			return nil, fmt.Errorf("env file %s:%d: invalid key %q", path, i+1, key)
		}
		pairs = append(pairs, envPair{key: key, value: unquote(strings.TrimSpace(value))})
	}
	return pairs, nil
}

func validEnvKey(key string) bool {
	if key == "" {
		return false
	}
	for i, r := range key {
		switch {
		case r == '_' || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z'):
		case r >= '0' && r <= '9':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func unquote(v string) string {
	if len(v) >= 2 {
		if (v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'') {
			return v[1 : len(v)-1]
		}
	}
	return v
}

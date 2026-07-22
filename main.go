package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"
)

// Build metadata injected via -ldflags (see Makefile).
var (
	version   = "dev"
	gitCommit = "unknown"
	buildTime = "unknown"
)

// Exit codes, documented in README.md. A service being DOWN is not a program
// failure: when the alert is delivered the exit code is 0.
const (
	exitOK      = 0
	exitRuntime = 1
	exitConfig  = 2
	exitNotify  = 3
	exitState   = 4
)

// Sentinel errors map run() failures to exit codes.
var (
	errNotify = errors.New("alert delivery failed")
	errState  = errors.New("state persistence failed")
)

// runDeps carries the injectable side effects so tests can drive run()
// end-to-end with httptest servers, virtual clocks, and buffers.
type runDeps struct {
	client *http.Client
	sleep  func(time.Duration)
	now    func() time.Time
	out    io.Writer // --dry-run output
}

func main() {
	args := os.Args[1:]
	if wantsVersion(args) {
		printVersion(os.Stdout)
		return
	}
	cfg, err := parseConfig(args, os.LookupEnv)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printUsage(os.Stdout)
			return
		}
		fmt.Fprintf(os.Stderr, "osm-monitor: %v\nRun 'osm-monitor --help' for usage.\n", err)
		os.Exit(exitConfig)
	}
	setupLogging(cfg.verbose)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	deps := runDeps{
		client: &http.Client{Timeout: cfg.timeout},
		sleep:  sleepCtx(ctx),
		now:    time.Now,
		out:    os.Stdout,
	}
	if err := run(ctx, cfg, deps); err != nil {
		slog.Error("run failed", "error", err)
		switch {
		case errors.Is(err, errNotify):
			os.Exit(exitNotify)
		case errors.Is(err, errState):
			os.Exit(exitState)
		default:
			os.Exit(exitRuntime)
		}
	}
	os.Exit(exitOK)
}

// run executes one monitoring cycle: check every enabled service, alert on
// transitions, persist state. On alert-delivery failure the affected
// service's previous state entry is kept so the next cron run re-detects the
// transition and retries the alert (at-least-once delivery).
func run(ctx context.Context, cfg *config, deps runDeps) error {
	state := loadState(cfg.stateFile)
	services := buildServices(cfg)
	host := monitorHost()

	var alertsSent, alertsFailed int
	var heartbeatFailed bool
	summary := make([]any, 0, 2*len(services)+2)
	statuses := make([]serviceStatus, 0, len(services))

	for _, svc := range services {
		result := runCheck(ctx, deps.client, svc, cfg, deps.sleep)
		now := deps.now().UTC()
		summary = append(summary, svc.key, statusWord(result.healthy))
		statuses = append(statuses, serviceStatus{name: svc.name, healthy: result.healthy})

		var prev *serviceState
		if p, ok := state.Services[svc.key]; ok {
			prev = &p
		}
		tr := computeTransition(prev, result, now)

		var message string
		switch tr.kind {
		case transitionDown:
			message = buildDownMessage(svc, result, now, host)
			slog.Info("service went down", "service", svc.key, "detail", result.detail) // #nosec G706 -- detail is flattened to one bounded line by sanitizeDetail
		case transitionRecovered:
			message = buildRecoveredMessage(svc, result, tr.downSince, now, host)
			slog.Info("service recovered", "service", svc.key, // #nosec G706 -- static key and locally computed duration
				"outage", now.Sub(tr.downSince).Round(time.Second).String())
		case transitionNone:
			slog.Debug("no transition", "service", svc.key, "healthy", result.healthy) // #nosec G706 -- static key and boolean
		}

		if message != "" {
			if cfg.dryRun {
				_, _ = fmt.Fprintf(deps.out, "[dry-run] would send Webex alert:\n%s\n\n", message) // #nosec G705 -- terminal output, not HTML; detail is sanitized
			} else if err := sendWebex(ctx, deps.client, cfg.webexWebhookURL, message); err != nil {
				slog.Error("webex alert failed; keeping previous state for retry", // #nosec G706 -- sendWebex errors carry only the HTTP status or transport cause, never the URL
					"service", svc.key, "error", err)
				alertsFailed++
				continue // do not commit this service's new state
			} else {
				alertsSent++
			}
		}
		state.Services[svc.key] = tr.next
	}

	summary = append(summary, "alerts_sent", alertsSent)
	slog.Info("monitoring cycle complete", summary...) // #nosec G706 -- summary holds static service keys, up/down literals, and a counter

	// The heartbeat is independent of per-service state: a failed heartbeat
	// exits 3 for visibility but never blocks state commits, since the cycle
	// itself (and any transition alerts) already succeeded.
	if cfg.heartbeat {
		message := buildHeartbeatMessage(statuses, deps.now().UTC(), host)
		if cfg.dryRun {
			_, _ = fmt.Fprintf(deps.out, "[dry-run] would send Webex heartbeat:\n%s\n\n", message)
		} else if err := sendWebex(ctx, deps.client, cfg.webexWebhookURL, message); err != nil {
			slog.Error("webex heartbeat failed", "error", err) // #nosec G706 -- sendWebex errors carry only the HTTP status or transport cause, never the URL
			heartbeatFailed = true
		} else {
			slog.Info("heartbeat sent")
		}
	}

	notifyFailed := alertsFailed > 0 || heartbeatFailed
	if cfg.dryRun {
		return nil
	}
	if err := saveState(cfg.stateFile, state); err != nil {
		if notifyFailed {
			// A missed alert is the worse failure; report it, but log both.
			slog.Error("state save also failed", "error", err)
			return errNotify
		}
		return fmt.Errorf("%w: %v", errState, err)
	}
	if notifyFailed {
		return errNotify
	}
	return nil
}

func statusWord(healthy bool) string {
	if healthy {
		return "up"
	}
	return "down"
}

func setupLogging(verbose bool) {
	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))
}

// sleepCtx returns a sleep function that wakes early when ctx is canceled, so
// SIGINT/SIGTERM are not stuck behind a backoff delay.
func sleepCtx(ctx context.Context) func(time.Duration) {
	return func(d time.Duration) {
		if d <= 0 {
			return
		}
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-ctx.Done():
		case <-t.C:
		}
	}
}

func wantsVersion(args []string) bool {
	for _, arg := range args {
		switch arg {
		case "-v", "-version", "--version":
			return true
		}
	}
	return false
}

func printVersion(w io.Writer) {
	_, _ = fmt.Fprintf(w, "osm-monitor %s (commit %s, built %s, %s)\n",
		version, gitCommit, buildTime, runtime.Version())
}

func printUsage(w io.Writer) {
	_, _ = fmt.Fprintf(w, `osm-monitor — OpenStreetMap service availability monitor with Webex alerts

Checks the OSM API, Nominatim, and OpenRouteService, and posts to a Webex
incoming webhook when a service goes DOWN or comes back up (RECOVERED).
Designed to run as a cron job: per-service state is kept in a JSON file
between runs, so alerts fire only on status transitions.

Usage:
  osm-monitor [flags]

Flags (each falls back to the env var in brackets, then to the default):
  --webex-webhook-url URL  Webex incoming webhook URL; required unless --dry-run
                           [OSMMON_WEBEX_WEBHOOK_URL]
  --osm-url URL            OSM API capabilities URL; "" disables the check
                           [OSMMON_OSM_URL] (default %s)
  --nominatim-url URL      Nominatim status URL; "" disables the check
                           [OSMMON_NOMINATIM_URL]
  --ors-url URL            OpenRouteService directions URL; "" disables the check
                           [OSMMON_ORS_URL]
  --ors-api-key KEY        OpenRouteService API key, sent as Authorization header
                           [OSMMON_ORS_API_KEY, fallback ORS_API_KEY]
  --state-file PATH        JSON state file between runs [OSMMON_STATE_FILE]
                           (default %s)
  --timeout DUR            HTTP timeout per request [OSMMON_TIMEOUT] (default %s)
  --attempts N             Attempts per service before declaring DOWN
                           [OSMMON_ATTEMPTS] (default %d)
  --backoff DUR            Base delay between attempts, doubles per retry
                           [OSMMON_BACKOFF] (default %s)
  --user-agent UA          Identifying User-Agent per OSM usage policy
                           [OSMMON_USER_AGENT]
  --env-file PATH          Load environment defaults from a KEY=value file
  --heartbeat              Also post a heartbeat summary message for this
                           cycle (proof of life; use in a scheduled cron
                           entry, e.g. weekly)
  --dry-run                Check and print would-be alerts; no webhook POST,
                           no state write
  --verbose                Debug logging [OSMMON_VERBOSE]
  -v, --version            Print version and exit
  --help                   Print this help and exit

Exit codes:
  0 success (including "service down, alert delivered")
  1 unexpected runtime error   2 config/usage error
  3 Webex delivery failure     4 state persistence failure
`,
		defaultOSMURL, defaultStateFile, defaultTimeout, defaultAttempts, defaultBackoff)
}

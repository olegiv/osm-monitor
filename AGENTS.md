# AGENTS.md

Source of truth for AI agents working in this repository. If guidance here
conflicts with other docs, AGENTS.md wins.

## What this is

`osm-monitor` — a one-shot Go console app that checks OpenStreetMap-based
services (OSM API, Nominatim, OpenRouteService) and posts DOWN/RECOVERED
alerts to a Webex incoming webhook. It runs from cron on Linux; state between
runs lives in a JSON file. Development happens on macOS.

## Repo shape

Flat `package main` at the repo root:

- `main.go` — CLI entry, `run()` orchestration, exit codes, usage/version
- `config.go` — flag/env/env-file resolution and validation
- `checker.go` — HTTP probing with retry/backoff + per-service parsers
- `webex.go` — webhook sender + alert message builders
- `state.go` — state persistence + transition logic
- `*_test.go` — one test file per source file; `main_test.go` holds e2e tests

## Canonical workflows

```bash
make build       # dev build -> bin/osm-monitor
make test        # unit tests (always offline)
make check       # fmt-check build test vet lint staticcheck sec vuln
make dry-run     # live-endpoint smoke test, sends/writes nothing
make heartbeat   # real cycle + posts heartbeat to the webhook — never run
                 #   without an explicit user request (sends a real message)
make build-all-platforms  # linux-amd64 + darwin-arm64 release binaries
```

Run `make check` before declaring any change done.

## Safety rules

- **Never commit `.env`** or any webhook URL / API key. The Webex webhook URL
  is a bearer credential; the ORS key is a credential. Both are gitignored
  via `.env`.
- **Never log credentials.** `sendWebex` errors must not contain the webhook
  URL; checker details must not contain the ORS key. Tests assert this —
  keep them passing.
- **Tests stay offline.** All tests use `httptest`; only `make dry-run` /
  `make run` touch live endpoints.
- Zero third-party dependencies is a deliberate security decision — do not
  add modules without explicit approval.
- `.audit/` holds local security-audit output and is gitignored.
- Keep the identifying `User-Agent` default: the OSM/Nominatim usage policy
  requires it.

## Behavioral truths

- Healthy definitions: OSM API = HTTP 200 ∧ `api.status.api == "online"` ∧
  `api.status.database == "online"` (gpx not required). Nominatim = HTTP 200
  ∧ `status == 0`. ORS = HTTP 200 ∧ non-empty `features` or `routes`.
- Alerts fire **only on transitions** (state change + recovery, with outage
  duration). First sighting of a service: silent if UP, alert if DOWN.
  Detail drift while DOWN never re-alerts.
- `--heartbeat` runs the full normal cycle and additionally posts an
  unconditional per-service summary (proof of life). A failed heartbeat
  delivery exits 3 but never blocks per-service state commits; in
  `--dry-run` the heartbeat is printed, not sent. Intended schedule:
  weekly, Monday 09:55 `Europe/Zurich` (see README for the exact cron
  line and the flock interplay with the 5-minute run).
- **At-least-once alerts**: on webhook failure the service's new state is not
  committed (`continue` in `run()`), so the next run retries. Exit code 3.
- Exit codes: 0 success (even if a service is down — alert delivered),
  1 runtime, 2 config, 3 webhook delivery (wins over 4), 4 state persist.
- Config precedence: flags > process env > `--env-file` > defaults. ORS key:
  `OSMMON_ORS_API_KEY` wins over `ORS_API_KEY`. Empty service URL disables
  that check; disabling all three is a config error.
- State file writes are atomic (temp + rename, mode 600). A missing/corrupt/
  unknown-schema state file means first-run semantics, never a crash.
- Build outputs go to `bin/`; version info is injected via ldflags
  (`main.version`, `main.gitCommit`, `main.buildTime`).

# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.1.0] - 2026-07-22

### Added

#### Monitoring
- One-shot availability monitor for the OSM API, Nominatim, and
  OpenRouteService, designed for cron: per-service state is kept in a
  JSON file between runs, so alerts fire only on status transitions.
- Webex incoming-webhook alerts: 🔴 DOWN with failure reason and attempt
  count, 🟢 RECOVERED with outage duration.
- `--heartbeat` flag (and `make heartbeat`) posting a per-cycle ✅/❌
  summary as proof of life for the cron pipeline.
- Per-service retries with exponential backoff (`--attempts`,
  `--backoff`), bounded HTTP timeouts, and `--dry-run` printing would-be
  alerts without webhook posts or state writes.
- Configuration precedence flags > env > `--env-file` > defaults, and
  documented exit codes (0/1/2/3/4) so cron MAILTO catches config,
  delivery, and state failures.

#### Project
- GPL-3.0 license.
- GitHub Actions CI running the full quality gate (fmt, build, test,
  vet, golangci-lint, staticcheck, gosec, govulncheck).

### Security
- Retry knobs are bounded (`--attempts` ≤ 10, `--backoff` ≤ 5m) and the
  backoff doubling is overflow-proof, so a misconfiguration can no
  longer produce a multi-day sleep that silently blocks subsequent cron
  runs (APP-001; ships with regression tests).
- Remote-derived text in alerts renders as a literal markdown code span,
  so upstream response content cannot inject links or formatting into
  the Webex channel (APP-002; ships with regression tests).
- Credential-bearing URLs (`--webex-webhook-url`, `--ors-url`) must use
  https; plain http is rejected at startup (APP-003; ships with
  regression tests).
- Documented systemd deployment runs as a dedicated sandboxed user
  instead of root (CFG-002); CI actions are pinned to commit SHAs and
  tools to exact versions (CFG-004).

[Unreleased]: https://github.com/olegiv/osm-monitor/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/olegiv/osm-monitor/releases/tag/v0.1.0

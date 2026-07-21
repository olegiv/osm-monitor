# CLAUDE.md

Read and follow [AGENTS.md](AGENTS.md) — it is the source of truth for
workflows, safety rules, and behavioral guarantees in this repo.

## Commands

```bash
make build       # dev build -> bin/osm-monitor
make test        # offline unit tests
make coverage    # coverage summary
make check       # all quality gates: fmt-check build test vet lint
                 #   staticcheck sec vuln
make dry-run     # live smoke test; no webhook POST, no state write
```

## Architecture in one paragraph

One-shot monitoring cycle: `main()` parses config (flags > env > `--env-file`
> defaults, see `config.go`), then `run()` loads the JSON state file, probes
each enabled service sequentially with retries (`checker.go`), computes
per-service transitions (`state.go`), sends Webex markdown alerts on
DOWN/RECOVERED (`webex.go`), persists state atomically, and exits with a
documented code (0/1/2/3/4). On webhook failure the service's state is not
committed so the next cron run retries the alert.

## Testing seams

`run()` takes a `runDeps` struct (`http.Client`, `sleep`, `now`, `out`) —
inject `httptest` servers, a recorded sleep, a fixed clock, and a buffer.
`parseConfig` takes an `envLookup` func — inject a map instead of real env.
Parsers (`parseOSMCapabilities`, `parseNominatimStatus`,
`parseORSDirections`) and `computeTransition` are pure — table-test them
directly. All tests stay offline.

## Local notes

- `.env` here contains a real `ORS_API_KEY` — never commit or print it.
- `make dry-run` is the safe way to verify against live endpoints.
- Commits require explicit user approval first (global workflow rule).

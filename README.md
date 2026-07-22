# osm-monitor

Console monitor for OpenStreetMap-based services with Webex alerts. Designed
to run as a cron job on Linux and be developed on macOS.

Each run checks the enabled services, compares the result with the previous
run (persisted in a JSON state file), posts a Webex message when a service
goes **DOWN** or **RECOVERS**, saves the new state, and exits.

## Monitored services

| Service | Default check | Healthy when |
|---|---|---|
| OSM API | `GET /api/0.6/capabilities.json` | HTTP 200 and `api.status.api` **and** `api.status.database` are `"online"` (`gpx` is informational) |
| Nominatim | `GET /status?format=json` | HTTP 200 and `status == 0` |
| OpenRouteService | Minimal authenticated directions request | HTTP 200 with a non-empty `features`/`routes` result — proves the API key, quota, and routing engine end-to-end |

Set a service URL to `""` to disable that check. Within a run each service
gets up to `OSMMON_ATTEMPTS` tries with doubling backoff before it is
declared DOWN, so transient blips do not page anyone.

## Quick start

```bash
cp .env.example .env   # then fill in the webhook URL and ORS API key
make build
make dry-run           # checks services, prints would-be alerts, sends nothing
make run               # real monitoring cycle
```

Zero third-party Go dependencies — standard library only.

## Creating the Webex incoming webhook

The monitor posts alerts through a Webex **incoming webhook** — a URL that
accepts POSTed messages into one space. One-time setup:

1. **Pick or create the target space** in the Webex app (`Cmd+Shift+N` /
   `Ctrl+Shift+N` → name it, e.g. *OSM Monitoring* → **Create**), and add the
   teammates who should see the alerts.
2. **Open the Incoming Webhooks page on Webex App Hub**:
   <https://apphub.webex.com/applications/incoming-webhooks-cisco-systems-38054-23307-75252>
   and sign in with your corporate Webex account (click **Connect** if asked
   to authorize the app).
3. **Create the webhook**: in the webhook form enter a name (e.g.
   `osm-monitor` — this becomes the sender name shown in the space), select
   the target space, and click **Add**.
4. **Copy the generated webhook URL** from the new entry (expand it if
   collapsed). It looks like
   `https://webexapis.com/v1/webhooks/incoming/Y2lzY29zcGFy...`.
5. **Put it in `.env`** on the machine that runs the monitor:

   ```sh
   OSMMON_WEBEX_WEBHOOK_URL="https://webexapis.com/v1/webhooks/incoming/<your-id>"
   ```

6. **Test it** — the message should appear in the space within seconds:

   ```bash
   set -a; . ./.env; set +a; curl -sS -o /dev/null -w '%{http_code}\n' -X POST -H 'Content-Type: application/json' -d '{"markdown":"✅ **osm-monitor** webhook test"}' "$OSMMON_WEBEX_WEBHOOK_URL"
   ```

   Any `2xx` status (usually `204`) means success.

Notes:

- **The webhook URL is a credential** — anyone who has it can post into the
  space. Keep it only in `.env` (gitignored here, `chmod 600` on servers).
  To rotate it, delete the webhook on the App Hub page and create a new one,
  then update `.env`.
- In managed corporate orgs the integration may be disabled by policy. If
  App Hub refuses to add the webhook, ask your Webex org admin to enable the
  **Incoming Webhooks** app in Control Hub (Apps → search for "Incoming
  Webhook").
- Incoming webhooks accept plain text or markdown only (no cards, no
  @-mentions) — exactly what this monitor sends.

## Configuration

Precedence: **flags > environment > `--env-file` entries > defaults**.

| Flag | Env var | Default |
|---|---|---|
| `--webex-webhook-url` | `OSMMON_WEBEX_WEBHOOK_URL` | — (required unless `--dry-run`) |
| `--osm-url` | `OSMMON_OSM_URL` | `https://api.openstreetmap.org/api/0.6/capabilities.json` |
| `--nominatim-url` | `OSMMON_NOMINATIM_URL` | `https://nominatim.openstreetmap.org/status?format=json` |
| `--ors-url` | `OSMMON_ORS_URL` | ORS `v2/directions/driving-car` sample route |
| `--ors-api-key` | `OSMMON_ORS_API_KEY`, fallback `ORS_API_KEY` | — (required while ORS enabled) |
| `--state-file` | `OSMMON_STATE_FILE` | `./osm-monitor-state.json` |
| `--timeout` | `OSMMON_TIMEOUT` | `10s` per request |
| `--attempts` | `OSMMON_ATTEMPTS` | `3` |
| `--backoff` | `OSMMON_BACKOFF` | `5s` base, doubles per retry |
| `--user-agent` | `OSMMON_USER_AGENT` | `iru-osm-monitor/<version> (+contact)` |
| `--verbose` | `OSMMON_VERBOSE` | off |
| `--env-file` | — | load a shell-compatible `KEY=value` file |
| `--heartbeat` | — | also post a heartbeat summary for this cycle |
| `--dry-run` | — | no webhook POST, no state write |

The OSM and Nominatim [usage policies](https://operations.osmfoundation.org/policies/api/)
require an identifying `User-Agent` with contact info — keep it set.

## Alert policy

- Alert **only on transitions**: one message when a service goes DOWN, one
  when it RECOVERS (with outage duration). No spam while it stays down.
- First run (no state entry): silent if UP, alerts if DOWN.
- **At-least-once delivery**: if the Webex POST fails, that service's state
  is *not* committed, so the next cron run re-detects the transition and
  retries the alert. The run exits 3 to make the failure visible (MAILTO).
- A changed failure reason while a service stays DOWN never re-alerts.

Example messages:

> 🔴 **DOWN — OSM API**
> - **Reason:** api=readonly database=online gpx=online
> - **Checked:** 2026-07-21 12:05:00 UTC (3 attempt(s))
> - **Monitor:** monitor-host01

> 🟢 **RECOVERED — Nominatim**
> - **Status:** OK (data updated 2026-07-21T11:58:02+00:00)
> - **Down since:** 2026-07-21 11:45:00 UTC — **outage duration: 20m0s**
> - **Recovered:** 2026-07-21 12:05:00 UTC
> - **Monitor:** monitor-host01

## Heartbeat

Because alerts fire only on transitions, a silently broken cron pipeline
looks identical to "everything is up". A `--heartbeat` run performs the
normal cycle (checks + transition alerts) and **additionally** posts an
unconditional summary — proving cron, the checks, and the webhook are all
alive end-to-end:

> 💓 **HEARTBEAT — osm-monitor**
> - **Services:** OSM API ✅ · Nominatim ✅ · OpenRouteService ✅
> - **Checked:** 2026-07-27 07:55:03 UTC
> - **Monitor:** monitor-host01 (osm-monitor v1.0.0)

The heartbeat is scheduled **weekly: Monday 09:55 Geneva time**
(`Europe/Zurich`). Two details make this exact:

- `CRON_TZ=Europe/Zurich` pins the schedule to Geneva wall-clock time across
  DST changes regardless of the server's timezone. It is supported by cronie
  (RHEL/Alma/Rocky/Fedora) and recent Debian/Ubuntu cron; if your cron lacks
  it, use the systemd timer variant below or run the host in
  `Europe/Zurich`.
- 09:55 lands on the `*/5` grid, so the regular run fires in the same
  minute. The heartbeat line therefore uses a **blocking** `flock -w 240`
  (not `-n`): whichever of the two runs grabs the lock first proceeds, and
  both orders are correct — a heartbeat run performs the full normal cycle
  anyway, and the regular run either waits its turn or skips via its own
  `-n`. The weekly heartbeat is never lost.

```cron
CRON_TZ=Europe/Zurich
55 9 * * 1 flock -w 240 /tmp/osm-monitor.lock /opt/osm-monitor/bin/osm-monitor-linux-amd64 --heartbeat --env-file /opt/osm-monitor/.env --state-file /var/lib/osm-monitor/state.json >> /var/log/osm-monitor.log 2>&1 || echo "osm-monitor exited $?"
```

(With systemd instead: add a second service/timer pair for the heartbeat
with `OnCalendar=Mon *-*-* 09:55:00 Europe/Zurich`.)

A failed heartbeat delivery exits 3 (MAILTO visibility) but never blocks
state commits — the monitoring cycle itself already succeeded. If the
heartbeat stops arriving Monday mornings, the monitor host or webhook is
broken.

## Exit codes

| Code | Meaning |
|---|---|
| 0 | Success — including "service is DOWN and the alert was delivered" |
| 1 | Unexpected runtime error |
| 2 | Config/usage error |
| 3 | Webex delivery failure (state kept for retry; wins over 4) |
| 4 | State persistence failure |

## Build & quality gates

```bash
make build                # dev build -> bin/osm-monitor
make build-all-platforms  # bin/osm-monitor-linux-amd64 + -darwin-arm64
make test                 # unit tests (offline, httptest-based)
make coverage             # coverage summary
make check                # fmt-check + build + test + vet + golangci-lint
                          #   + staticcheck + gosec + govulncheck
```

## Deployment (cron on Linux)

```cron
MAILTO=ops@example.org
*/5 * * * * flock -n /tmp/osm-monitor.lock /opt/osm-monitor/bin/osm-monitor-linux-amd64 --env-file /opt/osm-monitor/.env --state-file /var/lib/osm-monitor/state.json >> /var/log/osm-monitor.log 2>&1 || echo "osm-monitor exited $?"
```

- `flock -n` skips a run if the previous one is still going.
- The `|| echo` prints only on non-zero exit, which is what triggers cron's
  MAILTO — so config, webhook, and state failures reach the operator even
  when Webex itself is unreachable.
- Create `/var/lib/osm-monitor/` writable by the cron user; `chmod 600` the
  server-side `.env`.

<details>
<summary>systemd timer alternative</summary>

`/etc/systemd/system/osm-monitor.service`:

```ini
[Unit]
Description=OSM availability monitor

[Service]
Type=oneshot
EnvironmentFile=/opt/osm-monitor/.env
ExecStart=/opt/osm-monitor/bin/osm-monitor-linux-amd64 --state-file /var/lib/osm-monitor/state.json
```

`/etc/systemd/system/osm-monitor.timer`:

```ini
[Unit]
Description=Run osm-monitor every 5 minutes

[Timer]
OnCalendar=*:0/5
RandomizedDelaySec=30
Persistent=true

[Install]
WantedBy=timers.target
```

Logs land in the journal; no flock needed (systemd serializes the unit).
</details>

## State file

Small JSON document (written atomically, mode 600) recording per-service
health, when the current status began, and the last check time. A missing or
corrupt state file is treated as a first run — it can never stop monitoring.

## Security notes

- The Webex webhook URL and the ORS API key are **credentials**: they live
  only in `.env` (gitignored) and are never logged; the ORS key is sent via
  the `Authorization` header, not in the URL.
- No third-party dependencies; `make check` runs gosec and govulncheck.

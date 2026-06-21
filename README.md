# github-scout

[![Image Size](https://ghcr-badge.egpl.dev/cplieger/github-scout/size)](https://github.com/cplieger/github-scout/pkgs/container/github-scout)
![Platforms](https://img.shields.io/badge/platforms-amd64%20%7C%20arm64-blue)
![base: Distroless](https://img.shields.io/badge/base-Distroless_nonroot-4285F4?logo=google)
[![Go Report Card](https://goreportcard.com/badge/github.com/cplieger/github-scout)](https://goreportcard.com/report/github.com/cplieger/github-scout)
[![Test coverage](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/cplieger/github-scout/badges/coverage.json)](https://github.com/cplieger/github-scout/actions/workflows/coverage.yml)
[![Mutation](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/cplieger/github-scout/badges/mutation.json)](https://github.com/cplieger/github-scout/issues?q=label%3Agremlins-tracker)
[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/cplieger/github-scout/badge)](https://scorecard.dev/viewer/?uri=github.com/cplieger/github-scout)

One cross-repo view of every GitHub Actions run that just broke — shipped to
Loki, rendered by a ready-made Grafana dashboard, with a click-through link
to each failure.

## The problem

If you have more than a handful of repositories, "did any of my CI, releases,
or scheduled jobs break?" is a surprisingly hard question to answer:

- The **Grafana GitHub datasource plugin** can list workflow runs for exactly
  one repository **and** one workflow file per query. There is no "all
  workflows, all repos" mode.
- GitHub's **org-level endpoints** don't help a personal account, and **private
  repos** have no cross-repo failure feed at all.
- The GitHub UI shows you failures **one repo at a time**, and email
  notifications are easy to tune out.

So a broken nightly job in a repo you haven't opened in a week stays broken,
silently. github-scout exists to close that gap with a single pane of glass.

## What it does

github-scout polls every repository it can see for a configured owner on a
schedule, finds the workflow runs that **failed** (build failures, failed
releases, timed-out scheduled jobs, startup failures), and emits each
newly-detected failure **once** as a structured JSON log line. Ship those lines
to Loki with Grafana Alloy (or any log collector) and the bundled dashboard
gives you:

- a **failed-runs table** — every failure across every repo, newest first, with
  a click-through link to the run on GitHub;
- a **failures-over-time** chart, stacked by repo, to spot a repo that suddenly
  starts failing;
- a **per-repo breakdown** of which repo needs the most attention;
- a **scout-health tile** so you know the watcher itself is alive.

It discovers repositories and workflows dynamically on every scan, so a new
repo — or a new workflow inside an existing one — is picked up automatically
with zero configuration changes.

## Design

### Logs, not metrics

A failed run is an **event** carrying rich, high-cardinality detail: a unique
run ID and URL, a workflow name, a branch, a trigger. That is log-shaped data,
not a numeric time-series. Modelling it as a Prometheus metric forces a bad
trade-off:

- a bare counter (`failed_runs_total 7`) tells you *how many* but nothing you
  can click — it loses the entire actionable payload; or
- an info-metric with the run URL/title as labels reintroduces the detail but
  abuses Prometheus with unbounded label cardinality and sticky stale series.

So github-scout writes structured logs instead. The dashboard still shows a
count (via a LogQL `count`), **and** every failure keeps its repo, workflow,
branch, and link. The guiding principle: surface **actionable items**, not
stats for stats' sake.

### Event-once semantics

Each run ID is emitted **exactly once** per process lifetime. A plain log count
over the events therefore equals the number of distinct failures, with no
deduplication gymnastics in the dashboard. The dedup set is an in-memory map of
run ID → creation time, pruned to the lookback window so memory stays bounded.

github-scout is **stateless** — there is no database and no on-disk state. The
dedup set lives in memory; history lives in Loki. A consequence: a process
restart can re-log failures still inside the lookback window. The dashboard's
count tiles deduplicate by run ID so counts stay correct, and at worst a row
appears twice in the raw table after a restart. Given failures are infrequent
and restarts rarer, this is a deliberate simplicity-for-robustness trade.

### Architecture

```
main.go                         composition root + jittered poll loop
  └─ internal/config            env-var parsing and validation
  └─ internal/github            GitHub REST client (repo + failed-run reads)
  └─ internal/collect           scan orchestrator: discover → list → dedup → emit
       └─ apiClient (interface) consumer-side seam; the github client satisfies it
  └─ internal/model             pure data types (Repo, FailedRun)
  └─ internal/urlsafe           URL path-segment safety predicate
```

Data flows in one direction each scan: `config` parameterises a `collect.Collector`,
which asks the `github.Client` to list the owner's repos, then for each repo
lists failed runs since `now − lookback`, deduplicates by run ID, and emits new
failures as `slog` JSON to stdout. Alloy ships stdout to Loki; Grafana queries
Loki. There is no HTTP server and no listening port.

The `collect` package depends on a small consumer-side `apiClient` interface
rather than the concrete client, so the orchestration logic is unit-tested with
a scripted fake and the HTTP client is tested separately against an `httptest`
server.

## Quick start

```yaml
services:
  github-scout:
    image: ghcr.io/cplieger/github-scout:latest
    container_name: github-scout
    restart: unless-stopped
    user: "1000:1000"

    environment:
      TZ: "Europe/Paris"
      GITHUB_OWNER: "your-login"        # user or org whose repos to scan
      GITHUB_TOKEN: "ghp_xxx"            # see token scopes below
      POLL_INTERVAL_MINUTES: "15"        # 0 = scan once then idle
      LOOKBACK_HOURS: "72"               # how far back to consider failures
      EXCLUDE_REPOS: ""                  # comma-separated bare repo names to skip
      LOG_LEVEL: "info"

    # Writable home for the /tmp/.healthy marker; the app is otherwise stateless.
    tmpfs:
      - "/tmp:size=1m,mode=1777,noexec,nosuid,nodev"
```

The image is published to `ghcr.io/cplieger/github-scout`. Pin a digest in
production.

### Token scopes

github-scout needs read access to repository metadata and Actions:

- **Classic PAT:** `repo` (to see private repositories) + `workflow` /
  `actions:read`.
- **Fine-grained PAT (recommended):** read-only, with **Contents: read** and
  **Actions: read** on the repositories you want scanned.

The token is only ever sent to `api.github.com` as a Bearer header and is never
logged (only its presence is logged at startup).

## Configuration reference

| Variable                | Description                                                                 | Default        | Required |
| ----------------------- | --------------------------------------------------------------------------- | -------------- | -------- |
| `GITHUB_OWNER`          | GitHub login (user or org) whose repositories are scanned                   | ``             | Yes      |
| `GITHUB_TOKEN`          | Personal access token (see scopes above)                                    | ``             | Yes      |
| `POLL_INTERVAL_MINUTES` | Minutes between scans. `0` scans once then idles (useful for debugging)     | `15`           | No       |
| `LOOKBACK_HOURS`        | How far back each scan considers runs (also bounds the dedup set)           | `72`           | No       |
| `EXCLUDE_REPOS`         | Comma-separated **bare** repo names to skip (silence known-noisy repos)     | ``             | No       |
| `LOG_LEVEL`             | `debug`, `info`, `warn`, `error`                                            | `info`         | No       |
| `TZ`                    | Container timezone                                                          | `Europe/Paris` | No       |

Out-of-range values are clamped (e.g. a poll interval beyond one year), and
invalid integers fall back to the default, so misconfiguration degrades safely
rather than crashing.

## Output

github-scout writes JSON to stdout, one line per newly-detected failure:

```json
{
  "time": "2026-06-21T12:00:03Z",
  "level": "INFO",
  "msg": "workflow run failed",
  "repo": "cplieger/vibekit",
  "workflow": "CI",
  "conclusion": "failure",
  "branch": "main",
  "event": "push",
  "run_number": 1060,
  "run_id": 12345678,
  "url": "https://github.com/cplieger/vibekit/actions/runs/12345678",
  "created_at": "2026-06-19T08:07:35Z"
}
```

`msg = "workflow run failed"` is the stable key the dashboard and any Loki ruler
alert filter on. The `conclusion` is one of `failure`, `timed_out`, or
`startup_failure`. Each scan also logs a `scan complete` summary line
(`scanned`, `skipped`, `new_failures`, `tracked`, `duration`); a repo-discovery
failure logs at `error` level.

## Grafana integration

Ship the container's stdout to Loki — Grafana Alloy's Docker log discovery does
this with no extra configuration — and import `grafana-dashboard.json`. The
dashboard uses a standard Loki datasource (no plugins); its core query is:

```logql
{container="github-scout"} | json | msg=`workflow run failed`
```

The table panel parses the JSON line into columns and renders the `url` field as
a click-through link. Because the events are plain logs, you can also write a
Loki ruler alert (`count_over_time(... [1h]) > 0`) to be notified the moment
anything breaks.

## Healthcheck

A marker file at `/tmp/.healthy` is written after each scan whose repo discovery
succeeded, and cleared otherwise. The `health` subcommand
(`/github-scout health`) checks the marker and exits non-zero when unhealthy —
this is the container's `HEALTHCHECK`, so no HTTP port or shell is needed on the
distroless image. The container starts unhealthy and flips healthy after the
first successful scan. Per-repo run-list failures are tolerated (logged; the
scan stays healthy); only a repo-discovery failure (bad token, rate limit) marks
the container unhealthy.

## Security

- **Distroless, rootless, no shell.** Runs as `nonroot` on
  `gcr.io/distroless/static` with no package manager or shell to exploit.
- **No listening port.** There is no HTTP server — nothing to reach from the
  network. Output is stdout; health is a file marker.
- **No persisted state.** The only filesystem write is the `/tmp/.healthy`
  marker on a small `noexec,nosuid,nodev` tmpfs.
- **Minimal supply chain.** No non-`cplieger` runtime dependencies; the
  `cplieger` `httpx` and `health` libraries provide retry/backoff and the health
  probe. Response bodies are capped with `io.LimitReader`; URL path segments
  built from input are validated to reject traversal and injection characters.
- **Secret hygiene.** The token is sent only to `api.github.com` and is never
  written to logs.

## Limitations

- **Failed Actions runs only (v1).** Pull requests, issues, code-scanning, and
  Dependabot alerts are intentionally out of scope — the Grafana GitHub
  datasource already handles those acceptably one repo at a time. The collector
  is structured so additional signal types can be added later (see
  [CONTRIBUTING.md](CONTRIBUTING.md)).
- **github.com only.** GitHub Enterprise Server would require making the API
  base URL configurable.
- **Re-emission on restart** (see *Event-once semantics* above).

## Development

Requires Go (see `go.mod` for the pinned version). From a clone:

```bash
go build ./...                              # compile
go test ./...                               # unit tests
go test -race ./...                         # race detector
go test ./internal/github -run=x -fuzz=FuzzDecodeRunsPage -fuzztime=30s  # fuzz the API decode
golangci-lint run ./...                     # lint (config synced from cplieger/ci)
```

To run it locally against your account, export a token and use one-shot mode:

```bash
GITHUB_TOKEN=ghp_xxx GITHUB_OWNER=your-login POLL_INTERVAL_MINUTES=0 LOG_LEVEL=debug go run .
```

See [CONTRIBUTING.md](CONTRIBUTING.md) for the architecture map, the extension
point for new signal types, and the contribution workflow.

## Dependencies

All dependencies are updated automatically via
[Renovate](https://github.com/renovatebot/renovate) and pinned by digest or
version for reproducibility.

| Dependency         | Source                                                            |
| ------------------ | ----------------------------------------------------------------- |
| golang             | [Go](https://hub.docker.com/_/golang)                             |
| Distroless static  | [Distroless](https://github.com/GoogleContainerTools/distroless)  |
| cplieger/httpx     | [httpx](https://github.com/cplieger/httpx) — retry/backoff client |
| cplieger/health    | [health](https://github.com/cplieger/health) — file-marker probe  |
| pgregory.net/rapid | [rapid](https://pkg.go.dev/pgregory.net/rapid) — tests only       |

## Credits

An original tool building on the
[GitHub REST API](https://docs.github.com/en/rest). The API-client design (auth
headers, the API-version pin, page-count pagination) follows patterns from the
MIT-licensed [githubexporter/github-exporter](https://github.com/githubexporter/github-exporter)
and [xrstf/github_exporter](https://github.com/xrstf/github_exporter) — no code
was copied verbatim; see [NOTICE](NOTICE) for attribution.

## Contributing

Issues and pull requests are welcome. github-scout is deliberately small and
single-purpose, so please open an issue before starting anything larger than a
bug fix. See [CONTRIBUTING.md](CONTRIBUTING.md) for the architecture map, local
setup, testing conventions, and the step-by-step extension point for adding new
signal types.

## Disclaimer

These images are built with care and follow security best practices, but they
are intended for **homelab use**. No guarantees of fitness for production
environments. Use at your own risk.

This project was built with AI-assisted tooling. The human maintainer defines
architecture, supervises implementation, and makes all final decisions.

## License

[GNU General Public License v3.0](LICENSE).

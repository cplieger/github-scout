# github-scout

[![Image Size](https://ghcr-badge.egpl.dev/cplieger/github-scout/size)](https://github.com/cplieger/github-scout/pkgs/container/github-scout)
![Platforms](https://img.shields.io/badge/platforms-amd64%20%7C%20arm64-blue)
![base: Distroless](https://img.shields.io/badge/base-Distroless_nonroot-4285F4?logo=google)
[![Go Report Card](https://goreportcard.com/badge/github.com/cplieger/github-scout)](https://goreportcard.com/report/github.com/cplieger/github-scout)
[![Test coverage](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/cplieger/github-scout/badges/coverage.json)](https://github.com/cplieger/github-scout/actions/workflows/coverage.yml)
[![Mutation](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/cplieger/github-scout/badges/mutation.json)](https://github.com/cplieger/github-scout/issues?q=label%3Agremlins-tracker)
[![OpenSSF Best Practices](https://www.bestpractices.dev/projects/13336/badge)](https://www.bestpractices.dev/projects/13336)
[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/cplieger/github-scout/badge)](https://scorecard.dev/viewer/?uri=github.com/cplieger/github-scout)
[![SBOM](https://img.shields.io/badge/SBOM-SPDX-1D4ED8)](https://github.com/cplieger/github-scout/releases)

One cross-repo view of everything that needs a look across your GitHub repos:
open pull requests, open issues, code-scanning alerts, and failed Actions runs,
shipped to Loki and rendered by a ready-made Grafana dashboard, with a
click-through link on every row.

## The problem

If you have more than a handful of repositories, "is anything waiting on me —
a stale PR, an open issue, a security alert, a broken nightly job?" is a
surprisingly hard question to answer:

- The **Grafana GitHub datasource plugin** can list workflow runs for exactly
  one repository **and** one workflow file per query. There is no "all
  workflows, all repos" mode.
- GitHub's **org-level endpoints** don't help a personal account, and **private
  repos** have no cross-repo feed at all.
- The GitHub UI shows you each of these **one repo at a time**, and email
  notifications are easy to tune out.

So a broken nightly job — or an open code-scanning alert — in a repo you
haven't opened in a week goes unnoticed. github-scout closes that gap with a
single pane of glass across every repo you own.

## What it does

github-scout polls every repository it can see for a configured owner on a
schedule and surfaces four actionable signals across all of them, each as a
structured JSON log line. Ship those lines to Loki with Grafana Alloy (or any
log collector) and the bundled dashboard gives you:

- **Open pull requests**: every open PR across every repo, newest first, with a
  click-through link (Renovate PRs filtered out by default);
- **Open issues**: every open issue, with labels and author (Renovate and
  auto-generated trackers filtered out by default);
- **Code-scanning alerts**: every open CodeQL / code-scanning alert, colour-
  coded by severity;
- **Failed Actions runs**: every failed, timed-out, or startup-failed run across
  all repos, newest first, with a click-through link;
- a **scout-health tile** so you know the watcher itself is still scanning.

It discovers repositories and workflows dynamically on every scan, so a new repo
(or a new workflow inside an existing one) is picked up automatically with zero
configuration changes.

## Design

### Logs, not metrics

A workflow run is an **event** carrying rich, high-cardinality detail: a unique
run ID and URL, a workflow name, a branch, a trigger. An open PR, issue, or
alert is likewise an **item** with a title, author, and link. That is log-shaped
data, not a numeric time-series. Modelling it as a Prometheus metric forces a
bad trade-off:

- a bare counter (`open_prs_total 7`) tells you _how many_ but nothing you
  can click — it loses the entire actionable payload; or
- an info-metric with the URL/title as labels reintroduces the detail but
  abuses Prometheus with unbounded label cardinality and sticky stale series.

So github-scout writes structured logs instead. The dashboard still shows a
count (via a LogQL `count`), **and** every row keeps its repo, title, and link.
The guiding principle: surface **actionable items**, not stats for stats' sake.

### Two emission models

The four signals split into two shapes:

- **Event-once** (Actions runs). A completed run happens at a point in time, so
  each run ID is emitted **exactly once**, as `msg="workflow run"` carrying its
  `conclusion`. A plain log count therefore equals the number of distinct runs;
  the dashboard filters by conclusion for the failures view. The dedup set is a
  map of run ID → creation
  time, pruned to the lookback window so it stays bounded. It lives in memory in
  the long-lived scheduled process and is also persisted to a small file
  (`/tmp/seen-runs.json`) so the same run is not re-emitted across one-shot
  `trigger` runs — see _State_ below.
- **Snapshot** (open PRs, open issues, code-scanning alerts). These are current
  _state_: an item stays open across scans, so github-scout re-emits the full
  current set **every scan**. When an item is closed / merged / fixed it simply
  stops appearing in later snapshots, and the dashboard reads the most recent
  scan as "what is open right now" (panels deduplicate by repo + number over a
  window slightly longer than the poll interval). No dedup state is needed.

github-scout keeps **no database** — history lives in Loki. The only cross-scan
state is the event-once dedup set (run ID → creation time, bounded to the
lookback window). In the long-lived scheduled/resident process it lives in
memory; under an external scheduler each `trigger` is a fresh process, so the
set is persisted to a small JSON file at `/tmp/seen-runs.json` and reloaded on
the next trigger. Because `/tmp` is shared across `docker exec` triggers of the
same running container, a run is emitted once and not re-emitted on the
following trigger. A cold start — the first run, or a container **recreate**
that clears `/tmp` — at worst re-logs runs still inside the lookback window; the
dashboard also dedups run counts by run ID, so counts stay correct either way.
Persistence is a best-effort optimisation, never a correctness dependency.

### Architecture

```
main.go                         composition root + jittered poll loop
  └─ internal/config            env-var parsing and validation
  └─ internal/github            GitHub REST client (repos, runs, PRs, issues, code scanning)
  └─ internal/collect           scan orchestrator: discover → collect signals → emit
       └─ apiClient (interface) consumer-side seam; the github client satisfies it
  └─ internal/model             pure data types (Repo, FailedRun, PullRequest, Issue, CodeScanningAlert)
  └─ internal/urlsafe           URL path-segment safety predicate
```

Data flows in one direction each scan: `config` parameterises a `collect.Collector`,
which asks the `github.Client` to discover the owner's repos, then collects open
PRs and issues with one cross-repo Search query each and walks the repos for
failed runs and code-scanning alerts. New failures are deduplicated by run ID;
the snapshot signals are emitted in full. Everything goes out as `slog` JSON to
stdout. Alloy ships stdout to Loki; Grafana queries Loki. There is no HTTP
server and no listening port.

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
      SCAN_INTERVAL: "15m"               # Go duration between scans; "off" = resident-idle
      LOOKBACK_HOURS: "72"               # how far back to consider failed runs
      EXCLUDE_REPOS: ""                  # comma-separated bare repo names to skip
      LOG_LEVEL: "info"
      # Optional noise filters (defaults shown) — raw GitHub search qualifiers:
      # PR_EXCLUDE_QUERY: "-author:app/renovate"
      # ISSUE_EXCLUDE_QUERY: "-author:app/renovate -label:renovate -label:auto-generated"

    # Writable /tmp for the health marker + the dedup state file (seen-runs.json).
    tmpfs:
      - "/tmp:size=16m,mode=1777,noexec,nosuid,nodev"
```

The image is published to `ghcr.io/cplieger/github-scout`. Pin a digest in
production.

### Token scopes

github-scout reads four signals, so the token needs read access to repository
metadata, Actions, pull requests, issues, and code scanning. Either token type
works, and both keep discovery dynamic (new repos auto-included):

- **Classic PAT:** `repo` covers private **and** public repos for all four
  signals — or `public_repo` for public-only repositories. `workflow` and
  `security_events` are **not** separate requirements (`repo` already grants
  Actions and code-scanning read).
- **Fine-grained PAT (recommended):** Repository access = **All repositories**
  (so repos you create later are discovered automatically); Repository
  permissions, all **Read-only**: **Actions**, **Pull requests**, **Issues**,
  **Code scanning alerts**. **Metadata: Read** is added automatically and powers
  the repo listing. Avoid "Only select repositories": it freezes the set, so new
  repos silently stop being scanned.

github-scout degrades gracefully if a permission is missing: a repo without code
scanning (or a token lacking that permission) simply yields no alerts rather than
failing the scan. The token is only ever sent to `api.github.com` as a Bearer
header and is never logged (only its presence is logged at startup).

## Configuration reference

| Variable                | Description                                                                 | Default        | Required |
| ----------------------- | --------------------------------------------------------------------------- | -------------- | -------- |
| `GITHUB_OWNER`          | GitHub login (user or org) whose repositories are scanned                   | ``             | Yes      |
| `GITHUB_TOKEN`          | Personal access token (see scopes above)                                    | ``             | Yes      |
| `SCAN_INTERVAL`         | Gap between scans, a Go duration (`15m`, `1h`). `off` = resident-idle       | `15m`          | No       |
| `LOOKBACK_HOURS`        | How far back each scan considers failed runs (also bounds the dedup set)    | `72`           | No       |
| `EXCLUDE_REPOS`         | Comma-separated **bare** repo names to skip (silences all signals)          | ``             | No       |
| `LOG_LEVEL`             | `debug`, `info`, `warn`, `error`                                            | `info`         | No       |
| `TZ`                    | Container timezone                                                          | `Europe/Paris` | No       |

Out-of-range or unparseable values fall back to the default (a bad
`SCAN_INTERVAL` keeps scanning at 15m; an out-of-range `LOOKBACK_HOURS` is
clamped), so misconfiguration degrades safely rather than crashing.

### Run modes

github-scout matches the fleet's scheduled-app convention — an internal timer
or an external scheduler, your choice:

- **Scheduled** (`SCAN_INTERVAL=15m`, the default): an internal jittered timer
  drives the scans. Failed runs are deduplicated in memory and emitted once.
- **Resident-idle** (`SCAN_INTERVAL=off`): no internal timer. The container
  sits healthy and idle while an external scheduler runs `github-scout trigger`
  on its own cadence — e.g. an Ofelia `job-exec`, like the rest of the fleet.
- **Trigger** (`github-scout trigger`): one scan, then exit 0/1 — the target
  for that external scheduler, or a manual one-shot run.

Each `trigger` is an independent process, but the run dedup set is persisted to
`/tmp/seen-runs.json` (shared across `docker exec` triggers of the same running
container), so a trigger reloads the previous one's set and emits each completed
run exactly once — no re-emission across triggers. A container recreate clears
`/tmp` and at worst re-logs the lookback window once. The snapshot signals
(PRs / issues / alerts) re-emit the full open set every scan by design.

Two optional noise filters take raw GitHub search qualifiers, appended to the
cross-repo PR/issue searches:

- `PR_EXCLUDE_QUERY` — default `-author:app/renovate` (drops Renovate PRs).
- `ISSUE_EXCLUDE_QUERY` — default
  `-author:app/renovate -label:renovate -label:auto-generated` (drops Renovate
  and auto-generated trackers).

## Output

github-scout writes JSON to stdout, one line per item. A failed run looks like:

```json
{
  "time": "2026-06-21T12:00:03Z",
  "level": "INFO",
  "msg": "workflow run",
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

Each signal has a stable `msg` the dashboard and any Loki ruler alert filter on.
Every line also carries `repo`, `url`, and `created_at`:

- `workflow run` (event-once) — `workflow`, `conclusion`, `branch`, `event`, `run_number`, `run_id`
- `open pull request` (snapshot) — `number`, `title`, `author`, `draft`
- `open issue` (snapshot) — `number`, `title`, `author`, `labels`
- `code scanning alert` (snapshot) — `number`, `rule`, `severity`, `tool`

The `conclusion` is any completed-run outcome (`success`, `failure`,
`timed_out`, `startup_failure`, `cancelled`, `skipped`, or `neutral`); the
dashboard treats `failure` / `timed_out` / `startup_failure` as the failure set
(the failed-run count tile and the failures table). Each scan also logs a
`scan complete` summary line (`scanned`, `skipped`, `open_prs`, `open_issues`,
`code_alerts`, `new_runs`, `new_failures`, `tracked`, `duration`); a
repo-discovery failure logs at `error` level.

## Grafana integration

Ship the container's stdout to Loki (Grafana Alloy's Docker log discovery does
this with no extra configuration) and import `grafana-dashboard.json` (or drop
it into a file-based dashboard provider). The dashboard uses a standard Loki
datasource (no plugins) and is organised top to bottom in the order you ask
questions:

1. **At a glance**: four count tiles (open PRs, open issues, code-scanning
   alerts, and failed CI runs in the picker range).
2. **Open work**: linked tables of the open PRs, issues, and code-scanning
   alerts as of the most recent scan. The Created column shows relative age
   ("3 days ago") with a red-to-green gradient, so the stalest items stand out.
3. **Recent CI failures**: a linked table of failed, timed-out, and
   startup-failed runs in the selected time range (successful runs are omitted).
4. **Scout health**: a tile that flips to STALLED if no scan completed recently.

Two controls shape what you see:

- The **Snapshot window** variable sets how far back the open-work tables and
  their tiles read for the latest snapshot. It must be at least `SCAN_INTERVAL`;
  the default `30m` gives 2x headroom at the default 15m scan interval. It does
  not affect the failure panels.
- The **time picker** affects only the failed-runs tile and table; keep it
  within `LOOKBACK_HOURS` (default 72h), the furthest back each scan looks.

Every panel is built on a single Loki selector, for example:

```logql
{container="github-scout"} | json | msg=`open pull request`
```

Tables render the `url` field as a click-through link. Because the events are
plain logs, you can also write a Loki ruler alert
(`count_over_time(... [1h]) > 0`) to be notified the moment anything breaks.

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
- **Minimal state on tmpfs.** The only filesystem writes are the `/tmp/.healthy`
  marker and a small `/tmp/seen-runs.json` dedup file, both on a
  `noexec,nosuid,nodev` tmpfs — no database, no persistent volume.
- **Minimal supply chain.** No non-`cplieger` runtime dependencies; the
  `cplieger` `httpx` and `health` libraries provide retry/backoff and the health
  probe. Response bodies are capped with `io.LimitReader`; URL path segments
  built from input are validated to reject traversal and injection characters.
- **Secret hygiene.** The token is sent only to `api.github.com` and is never
  written to logs.

## Limitations

- **Dependabot alerts are out of scope.** The four signals github-scout surfaces
  (open PRs, open issues, code-scanning alerts, failed Actions runs) are the
  cross-repo views with no usable aggregation elsewhere. Dependabot has its own
  alerting and is intentionally left out; the collector is structured so more
  signal types can be added later (see [CONTRIBUTING.md](CONTRIBUTING.md)).
- **github.com only.** GitHub Enterprise Server would require making the API
  base URL configurable.
- **Re-emission on container recreate.** A recreate (not a plain restart) clears
  the `/tmp` dedup file, so the next scan re-logs runs still inside the lookback
  window once (see _State_ above).

## Development

Requires Go (see `go.mod` for the pinned version). From a clone:

```bash
go build ./...                              # compile
go test ./...                               # unit tests
go test -race ./...                         # race detector
go test ./internal/github -run=x -fuzz=FuzzDecodeRunsPage -fuzztime=30s  # fuzz the API decode
golangci-lint run ./...                     # lint (config synced from cplieger/ci)
```

To run it locally against your account, export a token and use the `trigger`
subcommand (one scan, then exit):

```bash
GITHUB_TOKEN=ghp_xxx GITHUB_OWNER=your-login LOG_LEVEL=debug go run . trigger
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

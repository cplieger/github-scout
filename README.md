# github-scout

[![Image Size](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/cplieger/github-scout/badges/size.json)](https://github.com/cplieger/github-scout/pkgs/container/github-scout)
![Platforms](https://img.shields.io/badge/platforms-amd64%20%7C%20arm64-blue)
![base: Distroless](https://img.shields.io/badge/base-Distroless_nonroot-4285F4?logo=google)
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

If you have more than a handful of repositories, "is anything waiting on me:
a stale PR, an open issue, a security alert, a broken nightly job?" is a
surprisingly hard question to answer:

- The **Grafana GitHub datasource plugin** can list workflow runs for exactly
  one repository **and** one workflow file per query. There is no "all
  workflows, all repos" mode.
- GitHub's **org-level endpoints** don't help a personal account, and **private
  repos** have no cross-repo feed at all.
- The GitHub UI shows you each of these **one repo at a time**, and email
  notifications are easy to tune out.

So a broken nightly job (or an open code-scanning alert) in a repo you
haven't opened in a week goes unnoticed. github-scout closes that gap.

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

A workflow run, an open PR, or an alert is an event with a payload: a title,
an author, a URL you want to click. That is log-shaped data, not a numeric
time-series; a Prometheus counter would say _how many_ and lose everything
actionable. So github-scout writes structured logs, and the dashboard shows
counts (via LogQL) while every row keeps its repo, title, and link.

### Two emission models

The four signals split into two shapes, and the split is how you read the
dashboard:

- **Event-once** (Actions runs). A completed run is emitted **exactly once**,
  as `msg="workflow run"` carrying its `conclusion`, so a plain log count
  equals the number of distinct runs. Dedup state survives restarts; a cold
  start at worst re-logs runs still inside the lookback window, and the
  dashboard also dedups by run ID, so counts stay correct either way.
- **Snapshot** (open PRs, open issues, code-scanning alerts). These are
  current _state_: the full set is re-emitted **every scan**, a closed item
  simply stops appearing, and the dashboard reads the most recent scan as
  "what is open right now".

github-scout keeps **no database**; history lives in Loki. Its only local
state is two disposable files under `/tmp`: the event-once dedup set and an
HTTP revalidation cache that lets unchanged GitHub resources answer without
charging the rate limit. Losing either costs at most one noisier, full-price
scan. Logs are `slog` JSON on stdout with UTC timestamps, zone-stable
regardless of the container's `TZ`.

## Quick start

```yaml
services:
  github-scout:
    image: ghcr.io/cplieger/github-scout:latest
    container_name: github-scout
    restart: unless-stopped

    environment:
      GITHUB_OWNER: "your-login"   # user or org whose repos to scan
      GITHUB_TOKEN: "ghp_xxx"      # see token scopes below
      SCAN_INTERVAL: "15m"         # Go duration between scans (always self-scheduled)
```

The image is published to `ghcr.io/cplieger/github-scout` and mirrored to
Docker Hub as `cplieger/github-scout`. Pin a digest in production.

### Token scopes

github-scout reads four signals, so the token needs read access to repository
metadata, Actions, pull requests, issues, and code scanning. Either token type
works, and both keep discovery dynamic (new repos auto-included):

- **Classic PAT:** `repo` covers private **and** public repos for all four
  signals, or `public_repo` for public-only repositories. `workflow` and
  `security_events` are **not** separate requirements (`repo` already grants
  Actions and code-scanning read).
- **Fine-grained PAT (recommended):** Repository access = **All repositories**
  (so repos you create later are discovered automatically); Repository
  permissions, all **Read-only**: **Actions**, **Pull requests**, **Issues**,
  **Code scanning alerts**. **Metadata: Read** is added automatically and powers
  the repo listing. Avoid "Only select repositories": it freezes the set, so new
  repos silently stop being scanned.

github-scout distinguishes "no data" from "couldn't check". A repo that has
never run code scanning returns 404, a benign no-data outcome; a token lacking
the code-scanning permission returns 403, which is surfaced as a warning and
marks the scan degraded rather than being read as zero alerts. A private repo
on a plan without GitHub Advanced Security always returns 403 on code
scanning; list it in `CODE_SCANNING_EXCLUDE_REPOS` to skip just that signal.
The token is only ever sent to `api.github.com` as a Bearer header and is
never logged (only its presence is logged at startup).

## Configuration reference

| Variable | Description | Default | Required |
| --- | --- | --- | --- |
| `GITHUB_OWNER` | GitHub login (user or org) whose repositories are scanned | _none_ | Yes |
| `GITHUB_TOKEN` | Personal access token (see scopes above) | _none_ | Yes |
| `SCAN_INTERVAL` | Gap between scans, a Go duration (`15m`, `1h`). No disable value | `15m` | No |
| `LOOKBACK_HOURS` | How far back each scan considers completed runs (also bounds the dedup set) | `72` | No |
| `EXCLUDE_REPOS` | Comma-separated **bare** repo names to skip (silences all signals) | _(unset)_ | No |
| `CODE_SCANNING_EXCLUDE_REPOS` | Comma-separated bare repo names to skip for code scanning only (others kept) | _(unset)_ | No |
| `PR_EXCLUDE_QUERY` | Raw GitHub search qualifiers appended to the open-PR search | `-author:app/renovate` | No |
| `ISSUE_EXCLUDE_QUERY` | Raw GitHub search qualifiers appended to the open-issue search | `-author:app/renovate -label:renovate -label:auto-generated` | No |
| `LOG_LEVEL` | `debug`, `info`, `warn`, `error` | `info` | No |

Out-of-range or unparseable values fall back to the default (a bad
`SCAN_INTERVAL` keeps scanning at 15m; an out-of-range `LOOKBACK_HOURS` is
clamped), so misconfiguration degrades safely rather than crashing. No value
disables scanning (`off` / `disabled` / `0` fall back to the default too);
for on-demand scans, use the `trigger` subcommand.

### Run modes

- **Scheduled** (`SCAN_INTERVAL=15m`, the default): an internal jittered timer
  drives the scans in the resident process. Failed runs are deduplicated in
  memory and emitted once.
- **Trigger** (`github-scout trigger`): one scan, then exit 0/1: the dev
  loop (`go run . trigger`), cron on a bare host, CI. Its output goes to the
  invoking context's stdout, exactly where a one-shot's output belongs.

There is no externally-scheduled container mode. github-scout's stdout **is**
the product: the dashboard, the alert rules, and every query in this README
consume the container's main-process log stream. A scan executed inside the
container by an external scheduler (`docker exec … trigger`) writes to the
exec session instead, so every signal it collects is invisible to all of
them. In a container, the internal timer is the scheduler.

The run dedup set persists across `trigger` processes, so each completed run is
still emitted exactly once (see _Two emission models_ above for the
`/tmp/seen-runs.json` details and the container-recreate caveat).

## Output

github-scout writes JSON to stdout, one line per item. A failed run looks like:

```json
{
  "time": "2026-06-21T12:00:03Z",
  "level": "INFO",
  "msg": "workflow run",
  "repo": "owner/example",
  "workflow": "CI",
  "conclusion": "failure",
  "branch": "main",
  "event": "push",
  "run_number": 1060,
  "run_id": 12345678,
  "url": "https://github.com/owner/example/actions/runs/12345678",
  "created_at": "2026-06-19T08:07:35Z"
}
```

Each signal has a stable `msg` the dashboard and any Loki ruler alert filter on.
Every line also carries `repo`, `url`, and `created_at`:

- `workflow run` (event-once): `workflow`, `conclusion`, `branch`, `event`, `run_number`, `run_id`
- `open pull request` (snapshot): `number`, `title`, `author`, `draft`
- `open issue` (snapshot): `number`, `title`, `author`, `labels`
- `code scanning alert` (snapshot): `number`, `rule`, `severity`, `tool`

The `conclusion` is any completed-run outcome (`success`, `failure`,
`timed_out`, `startup_failure`, `cancelled`, `skipped`, or `neutral`); the
dashboard treats `failure` / `timed_out` / `startup_failure` as the failure set
(the failed-run count tile and the failures table). Each scan also logs a
`scan complete` summary line (`scanned`, `skipped`, `open_prs`, `open_issues`,
`code_alerts`, `new_runs`, `new_failures`, `tracked`, `duration`), plus three
data-integrity fields: `errors` (how many signal collections failed this scan),
`degraded` (`true` when `errors > 0`, or when discovery returned zero repos so
nothing was scanned), and `failed_signals` (the comma-joined signals it could
not read, e.g. `code_scanning`). These distinguish a verified `0` ("checked,
nothing there") from an unverified `0` ("could not check"), which matters most
for the code-scanning security signal.

A repo-discovery failure logs at `error` level and fails a one-shot `trigger`
run (exit 1); no scan outcome flips container health (see _Healthcheck_). An
incidental per-repo failure (a transient error, or one private repo without
GitHub Advanced Security returning 403 on code scanning) marks the scan
`degraded` but is not paged; silence an always-403 private repo with
`CODE_SCANNING_EXCLUDE_REPOS`. A systemic failure (a rejected token, a rate
limit, a token that lost repo visibility, or a signal dark across every repo
that has it) escalates to a distinct `error`-level `scan degraded` line
carrying a machine `cause`, a human `reason`, and `failed_signals`, so an
alert fires on a scan that went blind. See
[CONTRIBUTING.md](CONTRIBUTING.md#systemic-failure-causes) for the full
`cause` enum and the exact escalation rules.

## Grafana integration

Ship the container's stdout to Loki (Grafana Alloy's Docker log discovery does
this with no extra configuration) and import `grafana-dashboard.json` (or drop
it into a file-based dashboard provider). The dashboard uses a standard Loki
datasource (no plugins) and is organised top to bottom in the order you ask
questions:

1. **At a glance**: four count tiles (open PRs, open issues, code-scanning
   alerts, and failed CI runs in the picker range).
2. **Open work**: linked tables of the open PRs, issues, and code-scanning
   alerts as of the most recent scan.
3. **Recent CI failures**: a linked table of failed, timed-out, and
   startup-failed runs in the selected time range (successful runs are omitted).
4. **Scout health**: a STALLED tile (red when no scan completed recently)
   alongside a **Scan Integrity** tile that is neutral while recent scans read
   every signal and turns red when one was degraded (a signal it could not
   read, so a `0` above may be unverified rather than confirmed empty), plus a
   **Recent scan problems** panel listing every warning and error behind it.

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

### Alerting

github-scout has no metrics endpoint; its operational state is in its JSON
logs. Ship the container's logs to Loki as above and evaluate these two rules
with [Loki's ruler](https://grafana.com/docs/loki/latest/alert/); firing alerts
deliver through your Alertmanager exactly like Prometheus metric alerts. The
first catches a scan that ran but went blind (a rejected token, a rate limit,
or a signal dark across every repo); the second is a deadman that fires when no
scan completes at all.

```yaml
groups:
  - name: github-scout
    rules:
      - alert: GithubScoutScanDegraded
        expr: |
          sum(count_over_time({container="github-scout"} |= `scan degraded` | json | msg=`scan degraded` [40m])) >= 2
        for: 0m
        labels:
          severity: warning
        annotations:
          summary: "github-scout scans degraded (signal counts unverified)"
          description: >
            github-scout logged repeated degraded scans in the last 40m: a
            signal could not be read, so the dashboard counts (especially Code
            Scanning Alerts) may read 0 because it could not check, not because
            nothing is there. The `scan degraded` log line carries the cause
            (token_invalid / rate_limited / no_repos_visible /
            code_scanning_blind / runs_blind / signal_blind) and the
            failed_signals field.
      - alert: GithubScoutScanStalled
        expr: |
          absent_over_time({container="github-scout"} |= `scan complete` [40m])
        for: 0m
        labels:
          severity: warning
        annotations:
          summary: "github-scout has not completed a scan in 40m"
          description: >
            No "scan complete" line from github-scout in 40m (it scans every
            ~15m by default). The scanner is wedged, the container is down, or
            the token was revoked at repo discovery, so every dashboard panel
            goes stale and silently reads empty. The Scan Integrity tile cannot
            flag this (no scan ran), so this liveness check does. Check the
            container and the GITHUB_TOKEN.
```

Thresholds and the `severity` label are starting points. Both windows assume
the default `SCAN_INTERVAL=15m` (40m is roughly 2.5 scan intervals), so widen
them if you lengthen `SCAN_INTERVAL`; adjust the `container` selector (or `job`
/ `service`, depending on your log collector) to your deployment, and route by
whatever labels your Alertmanager uses.

In a container the scan always runs as PID 1 (see _Run modes_), so these rules
apply to every containerized deployment as-is. If you instead run one-shot
`trigger` scans on a bare host (cron), the output lands in your scheduler's
stream rather than a `github-scout` container stream; point the selectors
there, and alert on the job's exit code for scan failures.

## Healthcheck

A marker file at `/tmp/.healthy` is the scan loop's **liveness** signal. The
daemon marks it healthy on boot (so a slow first scan on a large account never
holds the container unhealthy past the `HEALTHCHECK` start-period) and
refreshes it after every loop iteration, regardless of the scan's outcome. The
`health` subcommand (`/github-scout health`) exits non-zero when the marker is
missing or older than three scan intervals; this is the container's
`HEALTHCHECK`, so no HTTP port or shell is needed on the distroless image.

A wedged scan loop stops refreshing the marker and gets restarted by that
staleness deadline, the only failure class a restart repairs. Scan _outcomes_
(a bad token, a rate limit, a blind signal) never flip container health; they
are reported on the log channel instead (`repo discovery failed`,
`scan degraded`, and the absence of `scan complete`), which the bundled alert
rules page on. The one-shot `trigger` never touches the marker; its contract
is its exit code and its own stdout.

## Security

- **Distroless, rootless, no shell.** Runs as `nonroot` on
  `gcr.io/distroless/static` with no package manager or shell to exploit.
- **No listening port.** There is no HTTP server; nothing to reach from the
  network. Output is stdout; health is a file marker.
- **Minimal writable state.** The only filesystem writes are the `/tmp/.healthy`
  marker and two small state files (`/tmp/seen-runs.json` run dedup,
  `/tmp/cond-cache.json` HTTP revalidation cache); no database, no persistent
  volume. Under the hardened profile below, all three live on a
  `noexec,nosuid,nodev` tmpfs.
- **Minimal supply chain.** No non-`cplieger` runtime dependencies; the
  `cplieger` `httpx` and `health` libraries provide retry/backoff and the health
  probe. Response bodies are capped at 8 MB via `httpx`'s max-body limit; URL path segments
  built from input are validated to reject traversal and injection characters.
- **Secret hygiene.** The token is sent only to `api.github.com` and is never
  written to logs.

### Hardened deployment

To lock the container down further, layer these directives onto the Quick
start service:

```yaml
    read_only: true
    cap_drop:
      - ALL
    security_opt:
      - no-new-privileges:true
    tmpfs:
      - "/tmp:size=16m,mode=1777,noexec,nosuid,nodev"
```

With `read_only: true`, the writable `/tmp` state above (the health marker and
the two state files) needs the tmpfs; `size=16m` covers all three. Without
`read_only`, no tmpfs is needed.

## Limitations

- **Dependabot alerts are out of scope.** Dependabot has its own alerting and
  is intentionally left out; the collector is structured so more signal types
  can be added later (see [CONTRIBUTING.md](CONTRIBUTING.md)).
- **github.com only.** GitHub Enterprise Server would require making the API
  base URL configurable.
- **Re-emission on container recreate.** A recreate (not a plain restart) clears
  the `/tmp` dedup file, so the next scan re-logs runs still inside the lookback
  window once (see _Two emission models_ above).

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

| Dependency | Source |
| --- | --- |
| golang | [Go](https://hub.docker.com/_/golang) |
| Distroless static | [Distroless](https://github.com/GoogleContainerTools/distroless) |
| cplieger/httpx | [httpx](https://github.com/cplieger/httpx), retry/backoff client |
| cplieger/health | [health](https://github.com/cplieger/health), file-marker probe |
| cplieger/scheduler | [scheduler](https://github.com/cplieger/scheduler), poll loop, slot-file state |
| cplieger/slogx | [slogx](https://github.com/cplieger/slogx), slog setup |
| cplieger/envx | [envx](https://github.com/cplieger/envx), env-var getters |
| cplieger/runesafe | [runesafe](https://github.com/cplieger/runesafe), untrusted-string sanitizer |

## Credits

An original tool building on the
[GitHub REST API](https://docs.github.com/en/rest). The API-client design (auth
headers, the API-version pin, page-count pagination) follows patterns from the
MIT-licensed [githubexporter/github-exporter](https://github.com/githubexporter/github-exporter)
and [xrstf/github_exporter](https://github.com/xrstf/github_exporter). No code
was copied verbatim; see [NOTICE](NOTICE) for attribution.

## Contributing

Issues and pull requests are welcome. github-scout is deliberately small and
single-purpose, so please open an issue before starting anything larger than a
bug fix. See [CONTRIBUTING.md](CONTRIBUTING.md) for the architecture map, local
setup, testing conventions, and the step-by-step extension point for adding new
signal types.

## Disclaimer

This project is built with care and follows security best practices, but it is intended for personal / self-hosted use. No guarantees of fitness for production environments. Use at your own risk.

This project was built with AI-assisted tooling using [Claude](https://claude.com), [GPT](https://openai.com), and [Kiro](https://kiro.dev). The human maintainer defines architecture, supervises implementation, and makes all final decisions.

## License

GPL-3.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).

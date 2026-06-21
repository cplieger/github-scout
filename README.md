# github-scout

![Platforms](https://img.shields.io/badge/platforms-amd64%20%7C%20arm64-blue)
![base: Distroless](https://img.shields.io/badge/base-Distroless_nonroot-4285F4?logo=google)

Watch every one of your GitHub repositories for failed Actions runs and
surface them in one place — with a ready-made Grafana dashboard.

## What it does

The GitHub Grafana datasource plugin can show workflow runs only one repo
**and** one workflow file at a time, and private repos have no org-level
endpoint to aggregate over. So there is no built-in way to answer the one
question that actually matters day to day: *did anything break across any
of my repos?* github-scout fills that gap.

It scans every repository it can see for the configured owner on a
schedule, finds workflow runs that failed (build failures, failed
releases, timed-out scheduled jobs, startup failures), and emits each
newly-detected failure once as a structured JSON log line. Ship those
lines to Loki with Grafana Alloy (or any log collector) and the bundled
dashboard renders a single cross-repo "what just broke" table with a
click-through link to each run.

### Why logs, not metrics

A failed run is an **event** with rich, high-cardinality detail (a unique
run ID and URL, a workflow name, a branch). That is log-shaped data, not a
numeric time-series. Emitting it as a Prometheus metric would either lose
the actionable detail (a bare count tells you nothing to click) or abuse
labels with unbounded run URLs. So github-scout writes structured logs:
the dashboard still shows a count via a LogQL `count`, **and** every
failure keeps its repo, workflow, branch, and link. Built on the
philosophy of surfacing actionable items, not stats for stats sake.

### Why this design

- **Stateless** — no on-disk persistence. The dedup set that guarantees
  each failure is logged exactly once is in-memory and pruned to the
  lookback window. History lives in Loki.
- **Event-once semantics** — each run ID is emitted a single time, so a
  plain log count equals the number of distinct failures with no dedup
  gymnastics in the dashboard.
- **Dynamic** — repos are discovered every scan, so a new repo (or a new
  workflow inside one) is picked up automatically with no config change.
- **Private repos included** — uses the authenticated `/user/repos`
  endpoint, so failures in private repos are surfaced too (the datasource
  plugin and org-level endpoints cannot do this on a personal account).
- **Minimal dependencies** — no non-`cplieger` runtime deps; the
  `cplieger` `httpx` / `health` libraries supply retry/backoff and the
  health probe. Distroless, rootless, no shell.
- **No listening port** — there is no HTTP server. Output is stdout logs;
  health is a marker file checked by the `health` subcommand.

### Limitations

- **Failed Actions runs only (v1).** PRs, issues, code-scanning and
  Dependabot alerts are intentionally out of scope for now — the Grafana
  GitHub datasource already handles those acceptably. The collector is
  structured so additional signal types can be added later.
- **Token scope.** The PAT needs `repo` scope to see private repos and
  `actions:read` for workflow runs. A read-only fine-grained token works.
- **Re-emission on restart.** Because the dedup set is in-memory, a
  restart can re-log failures still inside the lookback window. The
  dashboard's count tiles deduplicate by run ID, so counts stay correct;
  at worst a row appears twice in the raw table after a restart.

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
      GITHUB_TOKEN: "ghp_xxx"            # repo + actions:read scope
      POLL_INTERVAL_MINUTES: "15"        # 0 = scan once then idle
      LOOKBACK_HOURS: "72"               # how far back to consider failures
      EXCLUDE_REPOS: ""                  # comma-separated bare repo names to skip
      LOG_LEVEL: "info"

    tmpfs:
      - "/tmp:size=1m,mode=1777,noexec,nosuid,nodev"
```

## Configuration reference

| Variable                | Description                                                                 | Default        | Required |
| ----------------------- | --------------------------------------------------------------------------- | -------------- | -------- |
| `GITHUB_OWNER`          | GitHub login (user or org) whose repositories are scanned                   | ``             | Yes      |
| `GITHUB_TOKEN`          | Personal access token (`repo` + `actions:read`)                             | ``             | Yes      |
| `POLL_INTERVAL_MINUTES` | Minutes between scans. `0` scans once then idles (debugging)                | `15`           | No       |
| `LOOKBACK_HOURS`        | How far back each scan considers runs (also bounds the dedup set)           | `72`           | No       |
| `EXCLUDE_REPOS`         | Comma-separated bare repo names to skip (silence known-noisy repos)         | ``             | No       |
| `LOG_LEVEL`             | `debug`, `info`, `warn`, `error`                                            | `info`         | No       |
| `TZ`                    | Container timezone                                                          | `Europe/Paris` | No       |

## Output

github-scout writes JSON to stdout. A failed run is one line:

```json
{"time":"2026-06-21T12:00:03Z","level":"INFO","msg":"workflow run failed","repo":"cplieger/vibekit","workflow":"CI","conclusion":"failure","branch":"main","event":"push","run_number":1060,"run_id":12345678,"url":"https://github.com/cplieger/vibekit/actions/runs/12345678","created_at":"2026-06-19T08:07:35Z"}
```

The `msg` field is the stable key the dashboard and any Loki ruler alert
filter on (`msg = "workflow run failed"`). Each scan also logs a `scan
complete` summary; repo-discovery failures log at `error`.

## Grafana integration

Ship the container's stdout to Loki (Grafana Alloy's Docker discovery does
this with no extra config) and import `grafana-dashboard.json`. The
dashboard queries `{container="github-scout"} | json` — a standard Loki
datasource, no plugins. It shows the failed-runs table with click-through
links, a failures-over-time chart, per-repo breakdown, and a scout-health
tile.

## Healthcheck

A marker file at `/tmp/.healthy` is written after each scan whose repo
discovery succeeded, and cleared otherwise. The `health` subcommand
(`/github-scout health`) checks it and exits 0 when healthy. The container
starts unhealthy and flips healthy after the first successful scan.
Per-repo run-list failures are tolerated (logged, scan stays healthy); a
repo-discovery failure (bad token, rate limit) marks it unhealthy.

## Credits

Original tool building on the [GitHub REST API](https://docs.github.com/en/rest).
The API-client design (auth headers, page-count pagination) follows
patterns from the MIT-licensed
[githubexporter/github-exporter](https://github.com/githubexporter/github-exporter)
and [xrstf/github_exporter](https://github.com/xrstf/github_exporter) —
see [NOTICE](NOTICE).

## Disclaimer

Built with care and security best practices, but intended for **homelab
use**. No guarantees of fitness for production. Use at your own risk.

This project was built with AI-assisted tooling. The human maintainer
defines architecture, supervises implementation, and makes all final
decisions.

## License

[GNU General Public License v3.0](LICENSE).

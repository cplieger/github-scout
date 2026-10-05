# Monitoring and alerts

This page lists the log lines github-scout writes, shows how to load and pin the Grafana dashboard, and gives two Loki alert rules. It is for operators who run Loki and Grafana.

## Log lines

github-scout writes JSON to standard output, one line per item. A failed run looks like this:

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

Each signal has a fixed `msg` that the dashboard and the alert rules filter on. Every item line also carries `repo`, `url` and `created_at`.

| `msg` | Logged | Other fields |
| --- | --- | --- |
| `workflow run` | once per finished run | `workflow`, `conclusion`, `branch`, `event`, `run_number`, `run_id` |
| `open pull request` | every scan | `number`, `title`, `author`, `draft` |
| `open issue` | every scan | `number`, `title`, `author`, `labels` |
| `code scanning alert` | every scan | `number`, `rule`, `severity`, `tool` |

The `conclusion` is any finished-run outcome: `success`, `failure`, `timed_out`, `startup_failure`, `cancelled`, `skipped` or `neutral`. The dashboard counts `failure`, `timed_out` and `startup_failure` as failures, in the failed-run tile and the failures table.

## Scan summary and integrity

After each scan, github-scout logs `scan complete` with `scanned`, `skipped`, `open_prs`, `open_issues`, `code_alerts`, `new_runs`, `new_failures`, `tracked` and `duration`. Three more fields tell a checked zero from a zero github-scout could not check:

- `errors` counts the signal reads that failed in this scan.
- `degraded` is `true` when `errors` is above 0, or when discovery found no repository, so nothing was scanned.
- `failed_signals` lists the signals it could not read, from `open_prs`, `open_issues`, `runs` and `code_scanning`.

A repository that has never run code scanning answers 404, which counts as no alerts. A token without the code-scanning permission answers 403, which is logged as a warning and marks the scan degraded, never read as zero alerts.

When discovery itself fails, github-scout logs `repo discovery failed` at `error` level and writes no `scan complete` line, and a one-shot `trigger` run exits 1. None of these outcomes makes the container unhealthy.

A failure in one repository, such as a passing network error or a private repository without GitHub Advanced Security, only marks the scan degraded. A failure that blinds a whole signal logs a separate `error` line, `scan degraded`, with a `cause`, a readable `reason`, `failed_signals` and `errors`.

| `cause` | Meaning |
| --- | --- |
| `token_invalid` | GitHub rejected the token with 401 and no signal could be read in this scan |
| `rate_limited` | GitHub kept answering 429 after the retries |
| `no_repos_visible` | Discovery succeeded but found no repository, so nothing was scanned |
| `code_scanning_blind` | Code scanning could not be read for any repository that has it |
| `runs_blind` | Actions runs could not be read for any scanned repository |
| `signal_blind` | The open pull request search or the open issue search failed |

A single 401 beside successful reads only marks the scan degraded, because GitHub returns occasional 401s under bursts even for a valid token. Repositories without code scanning and repositories skipped for code scanning are left out of the `code_scanning_blind` test, so they cannot hide a real outage.

## Grafana dashboard

Ship the container's logs to Loki with a `container` label that holds the container name, then import [`grafana-dashboard.json`](../grafana-dashboard.json) or drop it into a file-based dashboard provider. With Grafana Alloy's [`loki.source.docker`](https://grafana.com/docs/alloy/latest/reference/components/loki/loki.source.docker/), that label comes from a relabel rule that copies `__meta_docker_container_name` without its leading `/`. The dashboard reads from your default Loki data source and needs no plugin. Its rows follow the order you ask questions:

1. At a glance holds four count tiles, for open pull requests, open issues, code-scanning alerts and failed CI runs in the selected time range.
2. Open work holds linked tables of the open pull requests, issues and code-scanning alerts from the latest scan.
3. Recent CI failures holds a linked table of failed, timed-out and startup-failed runs in the selected time range. Successful runs are left out.
4. Scout health holds two tiles. **Scout Status** turns STALLED when no scan completed in the last hour. **Scan Integrity** turns red when a scan logged an error, so a zero above it was not checked.

Two controls shape what you see:

- The **Snapshot window** variable, under the dashboard's Settings then Variables, sets how far back the open-work tables and their tiles look for the latest scan. Keep it at least `SCAN_INTERVAL`. The default `30m` is twice the default scan interval. It does not change the failure panels.
- The time picker changes only the failed-run tile and table. Keep it within `LOOKBACK_HOURS`, 72 hours by default, which is the furthest back each scan reads.

Every panel uses one Loki selector, for example:

```logql
{container="github-scout"} | json | msg=`open pull request`
```

## Pinning the dashboard

The dashboard is versioned with the app. The JSON at release `<tag>` matches the log fields that image writes, and its `uid` stays the same, so a new import updates the dashboard in place. Pin it the way you pin the image, to the tag of the image you run. Each release publishes it in two forms:

- The release asset `https://github.com/cplieger/github-scout/releases/download/<tag>/grafana-dashboard.json`, with `grafana-dashboard.json.sha256` beside it. It works as grafana-operator `spec.url`, as the Grafana Helm chart's `dashboards.<provider>.<name>.url`, or as a Terraform `http` data source.
- The OCI artifact `ghcr.io/cplieger/github-scout/dashboard:<tag>`, for grafana-operator `spec.oci`.

Renovate can track either form, the URL with its `github-releases` datasource and the OCI tag with its `docker` datasource.

```yaml
apiVersion: grafana.integreatly.org/v1beta1
kind: GrafanaDashboard
metadata:
  name: github-scout
spec:
  instanceSelector:
    matchLabels:
      dashboards: grafana
  oci:
    reference: ghcr.io/cplieger/github-scout/dashboard:<tag>
    path: grafana-dashboard.json
```

## Alerting

github-scout has no metrics endpoint, so its state is in its logs. Ship the container's logs to Loki as above and evaluate these rules with [Loki's ruler](https://grafana.com/docs/loki/latest/alert/). Firing alerts go through your Alertmanager like any Prometheus alert. The first rule catches scans that ran but went blind. The second fires when no scan completes at all.

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
          summary: "github-scout scans are degraded, so signal counts are unverified"
          description: >
            github-scout logged repeated degraded scans in the last 40m.
            Dashboard counts may read 0 because a signal could not be read.
            Check the cause and failed_signals fields of the `scan degraded`
            log line.
      - alert: GithubScoutScanStalled
        expr: |
          absent_over_time({container="github-scout"} |= `scan complete` [40m])
        for: 0m
        labels:
          severity: warning
        annotations:
          summary: "github-scout has not completed a scan in 40m"
          description: >
            No `scan complete` line from github-scout in 40m, so every
            dashboard panel is stale. Check that the container is running,
            that its logs reach Loki, and that GITHUB_TOKEN is valid.
```

Notes on each rule:

- `GithubScoutScanDegraded` means a zero on the dashboard may not have been checked. The Code Scanning Alerts tile is the count most likely to read 0 this way. The `cause` field takes one of the values in the table under [Scan summary and integrity](#scan-summary-and-integrity).
- `GithubScoutScanStalled` expects a scan about every 15m by default. Repo discovery may be failing on a revoked or expired token, or the scan loop may have stopped. A stopped or renamed container, or a log pipeline that stopped shipping, also fires it. The Scan Integrity tile cannot flag a stall, because no scan ran.

Thresholds and the `severity` label are starting points. Both windows assume the default `SCAN_INTERVAL` of 15m, about 2.5 scan intervals, so widen them if you lengthen the interval. Change the `container` selector to the label your log collector sets, such as `job` or `service`, and route by whatever labels your Alertmanager uses.

In a container the scan always runs in the main process, so these rules apply to every container deployment as written. If you run one-shot `trigger` scans from cron on a host, the lines land in your scheduler's output and not in a `github-scout` container stream. Point the selectors there, and alert on the job's exit code for failed scans.

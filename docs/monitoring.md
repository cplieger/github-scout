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

The dashboard needs Grafana 13.2 or newer and the container's log in Loki with a `container` label that holds the container name, which the Alloy config in the [monitoring guide](https://github.com/cplieger/docs/blob/main/docs/monitoring.md#the-smallest-stack-sends-notifications-only) sets. [Importing an app's dashboard](https://github.com/cplieger/docs/blob/main/docs/monitoring.md#importing-an-apps-dashboard) shows how to load [`grafana-dashboard.json`](../grafana-dashboard.json). The dashboard reads from your default Loki data source and needs no plugin. Its rows follow the order you ask questions:

1. At a glance holds four count tiles, for open pull requests, open issues, code-scanning alerts and failed CI runs in the selected time range.
2. Open work holds linked tables of the open pull requests, issues and code-scanning alerts from the latest scan.
3. Recent CI failures holds a linked table of failed, timed-out and startup-failed runs in the selected time range. Successful runs are left out.
4. Scout health holds two tiles. **Scout status** turns Stalled when no scan completed in the last hour. **Scan integrity** turns red when a scan logged an error, so a zero above it was not checked.

Two controls shape what you see:

- The **Snapshot window** variable, under the dashboard's Settings then Variables, sets how far back the open-work tables and their tiles look for the latest scan. Keep it at least `SCAN_INTERVAL`. The default `30m` is twice the default scan interval. It does not change the failure panels.
- The time picker changes only the failure panels. Keep it within `LOOKBACK_HOURS`, 72 hours by default, which is the furthest back each scan reads. Each restart reports that whole window again, so a range can include a run that started before it.

Every panel uses one Loki selector, for example:

```logql
{container="github-scout"} | json | msg=`open pull request`
```

## Pinning the dashboard

The dashboard is versioned with the app. The JSON at release `<tag>` matches the log fields that image writes. The file sets `metadata.name` to `github-scout`, which Grafana uses as the dashboard UID. Because the name stays the same from release to release, importing a newer copy over the existing one, or a file provider reading the newer file, updates it in place. Pin it the way you pin the image, to the tag of the image you run. Each release publishes it in two forms:

- The release asset `https://github.com/cplieger/github-scout/releases/download/<tag>/grafana-dashboard.json`, with `grafana-dashboard.json.sha256` beside it. It works as the Grafana Helm chart's `dashboards.<provider>.<name>.url` with `curlOptions: "-sLf"`, because the release URL redirects, or as a Terraform `http` data source. Renovate can track the URL with its `github-releases` datasource.
- The OCI artifact `ghcr.io/cplieger/github-scout/dashboard:<tag>`, with the artifact type `application/vnd.grafana.dashboard.v2+json`.

On Grafana 13.1 or older, use the `grafana-dashboard.json` of release [v2.3.0](https://github.com/cplieger/github-scout/releases/tag/v2.3.0), the last one in the older dashboard format. That file gets no further changes, so you maintain it yourself.

The file is a Grafana dashboard resource with `apiVersion: dashboard.grafana.app/v2`, which grafana-operator's `GrafanaDashboard` does not accept. Load it with a `GrafanaManifest`, as grafana-operator's [dashboards v2 example](https://grafana.github.io/grafana-operator/docs/examples/manifests/dashboards-v2/) shows. A `GrafanaManifest` takes the dashboard inline and has no URL or OCI source. Copy the `spec` object of the release's `grafana-dashboard.json` in place of the comment below, and copy it again when you move to a new tag.

```yaml
apiVersion: grafana.integreatly.org/v1beta1
kind: GrafanaManifest
metadata:
  name: github-scout
spec:
  instanceSelector:
    matchLabels:
      dashboards: grafana
  template:
    apiVersion: dashboard.grafana.app/v2
    kind: Dashboard
    metadata:
      name: github-scout
    spec:
      # The spec object of grafana-dashboard.json, unchanged.
```

For a Grafana instance the operator does not manage, also set `namespace` under `template.metadata` to that instance's namespace. You can leave it out when the `Grafana` resource sets `tenantNamespace` in `spec.external`.

If a `GrafanaDashboard` already loads this dashboard from an older tag, keep it on that tag, including in any Renovate rule that bumps it. On grafana-operator v5.25.0, a `GrafanaDashboard` that receives a file in this format deletes the dashboard it manages. The issue is [grafana/grafana-operator#2955](https://github.com/grafana/grafana-operator/issues/2955). Replace it with the `GrafanaManifest` above.

With Terraform, the Grafana provider's [`grafana_apps_dashboard_dashboard_v2`](https://registry.terraform.io/providers/grafana/grafana/latest/docs/resources/apps_dashboard_dashboard_v2) resource takes the file's `spec` object as JSON, and the dashboard's name as `uid`:

```hcl
data "http" "github_scout_dashboard" {
  url = "https://github.com/cplieger/github-scout/releases/download/<tag>/grafana-dashboard.json"
}

resource "grafana_apps_dashboard_dashboard_v2" "github_scout" {
  metadata {
    uid = "github-scout"
  }
  spec {
    json = jsonencode(jsondecode(data.http.github_scout_dashboard.response_body).spec)
  }
}
```

## Alerting

github-scout has no metrics endpoint, so its state is in its logs. These rules are for Loki's ruler. Save the block below as a file in Loki's rules folder, as [Loading an app's alert rules](https://github.com/cplieger/docs/blob/main/docs/monitoring.md#loading-an-apps-alert-rules) shows. The first rule catches scans that ran but went blind. The second fires when no scan completes at all.

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

- `GithubScoutScanDegraded` means a zero on the dashboard may not have been checked. The Code scanning alerts tile is the count most likely to read 0 this way. The `cause` field takes one of the values in the table under [Scan summary and integrity](#scan-summary-and-integrity).
- `GithubScoutScanStalled` expects a scan about every 15m by default. Repo discovery may be failing on a revoked or expired token, or the scan loop may have stopped. A stopped or renamed container, or a log pipeline that stopped shipping, also fires it. The Scan integrity tile cannot flag a stall, because no scan ran.

Thresholds and the `severity` label are starting points. Both windows assume the default `SCAN_INTERVAL` of 15m, about 2.5 scan intervals, so widen them if you lengthen the interval. Change the `container` selector to the label your log collector sets, such as `job` or `service`, and route by whatever labels your Alertmanager uses.

In a container the scan always runs in the main process, so these rules apply to every container deployment as written. If you run one-shot `trigger` scans from cron on a host, the lines land in your scheduler's output and not in a `github-scout` container stream. Point the selectors there, and alert on the job's exit code for failed scans.

# Monitoring and alerts

This page lists the log lines forge-scout writes, shows how to load and pin the Grafana dashboard, and gives two Loki alert rules. It is for operators who run Loki and Grafana.

## Log lines

forge-scout writes JSON to standard output, one line per item. Every line carries `forge`, the product (`github`, `gitlab`, `gitea` or `forgejo`), and `connection`, the name from the config file. A line about no single connection, such as a startup line, carries `forge` `unknown` and an empty `connection`. A `security tool` line counts every GitHub connection of the scan together, so it carries `forge` `github` and an empty `connection`. A connection's lines before its product is detected carry `forge` `unknown`. A failed run looks like this:

```json
{
  "time": "2026-10-07T12:00:03Z",
  "level": "INFO",
  "msg": "ci run",
  "forge": "github",
  "connection": "github",
  "repo": "owner/example",
  "state": "failing",
  "workflow": "CI",
  "branch": "main",
  "default_branch": true,
  "trigger": "push",
  "run_id": 12345678,
  "run_number": 1060,
  "url": "https://github.com/owner/example/actions/runs/12345678",
  "created_at": "2026-10-07T11:50:35Z",
  "started_at": "2026-10-07T11:50:41Z",
  "updated_at": "2026-10-07T11:58:02Z",
  "duration_seconds": 441
}
```

Each signal has a fixed `msg` that the dashboard and the alert rules filter on. `repo` is the path the forge shows, such as `group/subgroup/project` on GitLab. Snapshot lines also carry `scan_id`, the time the scan started in Unix milliseconds, and `rank`, the line's place in its list across the scan, with `forge_rank` its place among the lines of the same forge. A capped list logs every line within its cap by `rank` or by `forge_rank`, so a table filtered to one forge still shows that forge's first lines.

| `msg` | Logged | Other fields |
| --- | --- | --- |
| `ci run` | when a finished run is first seen, and again each time its state or update time changes | `state`, `previous_state`, `workflow`, `branch`, `default_branch`, `trigger`, `run_id`, `run_number`, `url`, `created_at`, `started_at`, `updated_at`, `duration_seconds` |
| `open pull request` | every scan, idlest first | `number`, `ref`, `title`, `author`, `from`, `draft`, `labels`, `checks`, `url`, `created_at`, `updated_at`, `age_days`, `idle_days` |
| `open issue` | every scan, idlest first | `number`, `ref`, `title`, `author`, `from`, `labels`, `url`, `created_at`, `updated_at`, `age_days`, `idle_days` |
| `failing workflow` | every scan, the 25 failing longest overall and per forge | `workflow`, `branch`, `state`, `run_id`, `url`, `last_run_at`, `failing_since`, `failing_since_clipped`, `consecutive_failures` |
| `slow workflow` | every scan, the 10 slowest overall and per forge | `workflow`, `runs`, `p50_seconds`, `p95_seconds`, `url` |
| `ci day` | every scan, per repository its run lists read and each of the last 8 UTC days the run state holds for it | `day`, `passing`, `failing`, `neutral`, `timed_runs`, `run_seconds`, `clipped` |
| `disabled workflow` | every scan, GitHub only, the first 25 overall and per forge, those disabled for inactivity first | `workflow`, `path`, `state`, `url` |
| `security alert` | every scan, GitHub only | `source`, `number`, `rule`, `severity`, `tool`, `url`, `created_at` |
| `top security alert` | every scan, the 25 most severe overall and per forge | the `security alert` fields |
| `security tool` | every scan, GitHub only, the 10 tools with the most alerts across the scan, then one `rollup` line for the rest | `tool`, `alerts`, `rollup` |

Each value a line takes from the forge, such as a title, a label list, a repository path or an error, is cut at 4 KiB and then ends in `…`. So no line comes near Loki's default 256 KiB line limit. A link longer than 4 KiB, or one that is not an `http` or `https` address with a host, is left out. A line that cut a value or left out a link names those fields in `truncated`, such as `title,url`.

`state` is `passing`, `failing` or `neutral`, where neutral is a cancelled or skipped run. `previous_state` is absent on a run's first line and equals `state` when a re-run ended with the same result. `rollup` is `true` only on the line that adds up every tool past the first 10. That line carries no `tool`, so no tool name can stand for it. `trigger` is `push`, `pull_request`, `schedule`, `manual`, `other` or `unknown`. `from` is `you`, `bot` or `someone_else`. `checks` is the folded verdict of the pull request's head commit, `passing`, `failing`, `pending`, `neutral`, `none`, `unknown` or `unreadable`. It is absent on Gitea and Forgejo, whose pull request lists name no head commit. `none` means the read was whole and the head commit reports no checks. `unknown` means the read gave no verdict. Either it was cut short or failed, so a check left out may be failing, or a check reported a state forge-scout does not know. `unreadable` means GitHub refuses the token the pull request's checks, as it refuses check runs to a fine-grained token on a private repository. The dashboard shows it as Not available with this token. `last_run_at` on a `failing workflow` line is when its newest judged run on the branch was created. Its `state` is `failing`, or `unknown` when this scan's run list of the repository was cut short and that run is older than the oldest run the list read: the workflow may have run since, below the cut, so the line shows the last run forge-scout judged, which failed. `duration_seconds` and `started_at` are absent where the forge sends no start time. On a pull request or issue, `created_at` and `age_days` are absent where the forge sends no creation time, and `updated_at` and `idle_days` where it sends no update time. An item with no update time is listed after every item that has one. A `security alert` with no creation time carries no `created_at` and ranks after the dated alerts of its severity.

A `ci day` line counts the runs created on one UTC day in one repository, each once and in its newest state. `day` is the date the run was created, such as `2026-10-07`. `passing`, `failing` and `neutral` count the runs of each result, zeros included, so a re-run that passes moves its run from `failing` to `passing`. `timed_runs` counts the runs of the default branch with a start time, and `run_seconds` adds up their run time.

forge-scout keeps the runs of the last 8 days in the run state, whatever `lookback` is, so a `lookback` shorter than a day still counts whole days. `clipped` is `true` when no list covered part of the day, so the day holds at least those runs. That happens across a `runs coverage gap`, and on a new or emptied `/data` volume on the day its first list reaches back to. The days before that have no `ci day` line, so the charts show no bars for them until they pass out of the 8 days.

Each scan logs again the days of every repository its lists read, so take the newest line of each connection, repository and day, never the sum.

`ci run` lines are delivered at least once: a crash can repeat a run line, never drop one. forge-scout logs a connection's run lines before it saves them, so a crash between the two logs the same lines again after the restart. Count runs by `connection`, `repo`, `run_id`, `state` and `updated_at`, so a repeated line counts once.

## Scan summary and integrity

When a connection's repositories are listed, forge-scout logs `scanning` with `owners`, the number of configured owners, `repos`, the number of repositories it will read, and `since`, the start of the run lookback window. After each scan, it logs one `scan complete` line per connection with `scan_id` and these fields:

- The read state of each signal: `repos_read`, `open_prs_read`, `open_issues_read`, `runs_read`, `failing_workflows_read`, `pr_checks_read`, `security_alerts_read` and `disabled_workflows_read`.
- `repos_discovered`, `repos_truncated`, `skipped`, and `owners_empty`, the configured owners that resolved but answered no repository at all, comma-joined. It is absent when the repository list was cut short.
- `open_prs`, `open_issues`, `excluded_prs`, `excluded_issues`, `from_others_prs`, `from_others_issues`, `failing_checks_prs`, `checks_unread`, `checks_unsupported_for_token`, and the age counts `open_age_lt7d`, `open_age_7_30d`, `open_age_30_90d`, `open_age_90d_plus`, `open_age_unknown`, `idle_30d` and `idle_unknown`. An item the forge gave no creation or update time counts in `open_age_unknown` or `idle_unknown` instead of an age.
- `new_runs`, `changed_runs`, `new_failures`, `tracked`, `runs_failed_repos`, `runs_truncated_repos`, `runs_unread_repos`, `runs_unmapped`, `failing_workflows`, `failing_workflows_listed`, `slow_workflows_listed`, `run_durations_known`.
- On GitHub, the workflows read: `disabled_workflows`, `disabled_workflows_listed`, `workflows_failed_repos`, `workflows_truncated_repos`, `workflows_unread_repos`. The code-scanning alerts: `security_alerts`, `security_alerts_listed`, and the severity counts `security_alerts_critical`, `security_alerts_high`, `security_alerts_medium`, `security_alerts_low` and `security_alerts_unrated`. The code-scanning coverage: `security_repos_read`, `security_skipped`, `security_unavailable`, `security_unreadable`, `security_truncated_repos`, `security_unread_repos`.
- `errors`, `degraded`, `failed_signals`, `unsupported_signals`, `budget_remaining`, `budget_reset`, `duration_ms`, `scan_interval_s`.

`budget_remaining` and `budget_reset` are the budget the forge's last response reported. GitHub keeps separate budgets for its REST and GraphQL APIs, and each response reports only the one it drew on. On GitHub the figure is the REST budget, which every read of the connection shares, as the last response that reported it said, and it is absent once its window has renewed.

`duration_ms` is how long the connection's scan took, from its first read to the end of its save. `scan_interval_s` is the configured `scan_interval` in seconds, against which the dashboard judges whether the newest scan is late.

An owner that resolves but answers no repository leaves the read `complete`, because there is nothing to read, and logs `owner lists no repository` with `owners`, the same list as `owners_empty`. It logs at `warn` the first scan the list changes, then at `debug` while it stays the same. A typo in an owner name that names another account shows up there, as does a token that cannot see the owner's private repositories, and a GitLab user whose profile is private, which GitLab answers with an empty list. A connection that forge-scout refuses to open, because an earlier one uses the same account, logs `repo discovery failed` with cause `connection_failed`, as [Configuration](configuration.md) explains.

Each `_read` field is `complete`, `partial`, `blind`, `unread`, `unsupported` or `excluded`. `complete` means every read of the signal answered whole, and `blind` that no read answered. `unread` means the scan did not reach the signal, because it stopped first, the connection could not be opened or its repositories listed, or no owner resolved. `unsupported` means the forge does not have it, and `excluded` that the config file turns it off. A signal never reads more complete than the repository list it was read through.

`partial` means part of the signal is missing. A read failed or was cut short, a read was not sent after GitHub refused an earlier one, a row named a repository the listing did not return, an owner was not resolved, or the scan stopped while reading it. Items left out by `exclude_repos`, `exclude_authors`, `exclude_labels`, `skip_forks`, `include_private` or `skip_repos`, archived repositories, the workflows of forks and pull request checks the token cannot read leave a read `complete`.

`new_failures` counts the `ci run` lines logged this scan in the failing state, so a re-run that failed again counts too.

`tracked` is the number of runs whose last `ci run` line the run state holds, which is every finished run created in the last `lookback`. Each scan lists every repository's runs created in the last `lookback`. A re-run keeps its run's creation time, so a re-run is seen while its run is younger than `lookback`.

GitHub cuts a run list at 1,000 runs, and forge-scout stops one at 40 pages on any forge. A list cut short leaves `runs_read` `partial`, counts in `runs_truncated_repos` and logs `runs listing truncated` at `warn`. Its runs are logged and judged as usual. A workflow whose last judged run is older than the oldest run the cut list read stays listed with `state` `unknown`, and a failure streak that crosses the cut carries `failing_since_clipped` `true`. A cut list adds no `slow workflow` line, because its newest runs are no sample of the window.

When the last scan whose list of a repository's runs reached back to the scan before it started before the start of `lookback`, the runs created since were not all listed. That happens after the container was stopped for longer than `lookback`, or when the lists after that scan were cut short of it until the window passed it. forge-scout logs `runs coverage gap` at `warn` with the `repo`, `last_listed` and `hours`, the length of the gap rounded up to the hour. It logs it once, because the next list covers this one. The runs in the gap are never logged, and a failure streak after it carries `failing_since_clipped` `true`. The gap does not mark the scan degraded.

A list cut short also logs `runs coverage gap` once when the list before it read a run still in progress that was created before the cut list's oldest row, because that run may finish without any list reading it again. The line then carries `pending_since`, the creation time of the oldest such run, and `hours` is the span from it to the cut list's oldest row. Such a run is logged only if a later list reads it finished while it is younger than `lookback`. Otherwise its workflow keeps the result of its last finished run.

`failing_since_clipped` is `true` when a streak may have begun before `failing_since`: no list covered the run before its first failure. That is the case for a workflow's first runs on a new or emptied `/data` volume, after a coverage gap, and across a cut list. A workflow with no run in the last `lookback` keeps the result of its last run until 63 days after that run. `failing_workflows_read` is never more complete than `runs_read`.

A run in a state forge-scout cannot map, such as a conclusion GitHub added later, counts in `runs_unmapped` and logs `run state unknown` with the `repo` and the number of `runs`, at `warn` the first scan it is seen and at `debug` while it stays. It is not logged or judged, so its workflow keeps the result of its last judged run. A run still in progress is not logged either. Each scan reads both again while they are younger than `lookback`, and logs them once they finish in a state forge-scout maps. A run that takes longer than `lookback` to finish is never logged. `runs_read` stays `complete`, and the scan is not marked degraded.

How a workflow that no longer exists leaves the failing list depends on the forge. forge-scout learns of a workflow from its runs, and only GitHub also lists the workflows a repository defines. On GitHub, a failing workflow that a whole workflows list shows as `deleted` leaves the list at that scan. One the list no longer names, as after a rename, leaves it too, but only when an earlier whole list named it. GitHub titles each run of some workflows on its own, such as default-setup code scanning, Pages and Dependabot updates, so their run names never appear in the list and absence proves nothing for them. A workflows list that was cut short or failed proves nothing either, so the workflow stays listed. On GitLab, Gitea and Forgejo, and on a GitHub fork, whose workflows are not listed, a deleted or renamed workflow whose last run failed stays listed until 63 days after that run. Its `last_run_at` shows how old it is.

A run listing that holds a run with no creation time, or one updated before it was created, counts as a failed runs read for that repository and logs `runs listing failed` with the reason. None of that repository's runs are recorded that scan.

A count is absent when its signal was not read on that connection, so a missing number is never read as 0. When an open pull request or issue list could not be read whole, the connection logs none of its lines or counts. When `repos_truncated` is `true`, the connection logs none of its pull request, issue, code-scanning or workflows lines or counts, and keeps the coverage fields. When every run read fails or is not sent after GitHub refused an earlier one, it logs no failing or slow workflow, and a repository whose run read failed contributes none. When every code-scanning, workflows or pull request checks read fails or is not sent after GitHub refused an earlier one, the signal reads `blind`. Then `security_alerts` with its severity counts, `disabled_workflows` or `failing_checks_prs` is absent and the coverage fields stay. `failing_checks_prs` is also absent, and `pr_checks_read` `partial`, whenever `open_prs_read` is not `complete`, because a pull request left out may have failing checks. A code-scanning list cut short at 100 pages, which is 10,000 open alerts, or a workflows list cut short at 3 pages, on any repository works the same way: the connection logs none of that list's lines or counts, and `security_truncated_repos` or `workflows_truncated_repos` says how many repositories were cut. When a signal reads `blind` or `unread`, its counts are absent.

`unsupported_signals` names the signals the forge does not have: `security_alerts` and `disabled_workflows` on GitLab, Gitea and Forgejo, `pr_checks` on Gitea and Forgejo, and `runs` and `failing_workflows` on a Gitea whose run list carries no creation time. forge-scout sets it as soon as it knows the product, so a connection that fails or stops early still names them. `degraded` is `true` when a signal reads `partial` or `blind`, other than for a stopped scan, or when the run state could not be saved or made durable. `failed_signals` names those signals from `repos`, `open_prs`, `open_issues`, `runs`, `failing_workflows`, `pr_checks`, `security_alerts`, `disabled_workflows` and `run_state`.

A repository that has never run code scanning answers 404, which counts as no alerts. A 403 whose `X-RateLimit-Remaining` header is 0, or that carries a `Retry-After` header, is GitHub's primary or secondary rate limit, and stops the connection with `reason` `rate_limited`. Any other 403 on any GitHub read is a missing permission or a secondary rate limit, which GitHub tells apart only in its message, so forge-scout treats it as both. It logs the read's warning at `warn`, such as `runs listing failed`, `pull request checks unreadable`, `code scanning unreadable` or `workflows listing failed`, with the `repo`, the `error` and a `hint`. It then sends no further request on that connection for the rest of the scan, through either of its GitHub clients, and logs `scan degraded` with `cause` `github_refused`. A refused read is never read as zero. The reads not sent count as `runs_unread_repos`, `checks_unread`, `security_unread_repos` and `workflows_unread_repos`, and the next scan reads again. If a code-scanning refusal is expected, as for a private repository without GitHub Advanced Security while `security.include_private` is on, add the repository to `security.skip_repos`.

GitHub gives a fine-grained token no permission for check runs. On a private repository it then answers the open pull request list whole and refuses the token one pull request's checks, which is no failed read. That pull request's `checks` is `unreadable`, it counts in `checks_unsupported_for_token` and not in `failing_checks_prs`, and `pr_checks_read` stays `complete`, so the scan is not degraded. The first time a process meets such a pull request it logs `pull request checks not available with this token` once at `warn`, with the `repo`, the `number` and a `hint` naming the remedy, a classic token with the `repo` scope. Any other error on a pull request's checks is still a failed read.

When a connection cannot be opened or its repositories listed, forge-scout logs `repo discovery failed` at `error` level, then `scan degraded` and a `scan complete` line with `degraded` set, and a one-shot `trigger` run exits 1, as it does for any scan that is not complete. None of these outcomes makes the container unhealthy.

A failure that blinds a whole signal on a connection logs a separate `error` line, `scan degraded`, with `scan_id`, a `cause`, a readable `reason`, `failed_signals` and `errors`. It does so also when the scan then stops at the budget reserve. A failed checks or workflows read only marks the scan degraded, except a read GitHub refuses with 403, which logs `cause` `github_refused`. A workflows read also fails when GitHub answers 404 or reports a workflow state forge-scout does not know, so neither is counted as no disabled workflows.

| `cause` | Meaning |
| --- | --- |
| `token_invalid` | The forge rejected the token with 401 and no read succeeded on this connection during the scan |
| `scan_timeout` | The connection's scan ran past twice `scan_interval`, so it logged no open work, workflows, alerts or `scan complete`. The `ci run` lines it logged before the limit stand |
| `rate_limited` | The forge refused further requests |
| `state_unwritable` | The run state in `/data` could not be saved. This scan's `ci run` lines were already logged, and a restart before a save succeeds logs them again |
| `state_not_durable` | The run state in `/data` was written, but the file system could not sync its folder, so a power loss may log this scan's `ci run` lines a second time |
| `connection_failed` | The connection could not be opened or its repositories listed |
| `no_repos_visible` | The forge resolves none of the owners, so no repository was listed |
| `owner_unresolved` | The forge does not know one of the owners, which `reason` names |
| `github_refused` | GitHub answered a read with 403 and no rate-limit header, so the connection sent no further request that scan. The warning line before it names the read and the repository |
| `code_scanning_blind` | Code scanning could not be read for any repository that has it |
| `runs_blind` | CI runs could not be read for any repository |
| `signal_blind` | An open pull request or issue list could not be read whole |

A failed save also logs `run state save failed` at `error` level with the `path` and the `error`. A save that wrote the file but could not make it durable logs `run state not durable` at `error` level with the `path` and the `error` instead. Either way the scan's run changes were already logged, and the next save that succeeds stores them. A restart before that save logs them again.

A single 401 beside successful reads only marks the scan degraded, because GitHub returns occasional 401s under bursts even for a valid token.

When a connection's next read would take the budget its last response reported below the 250-request reserve, forge-scout stops that connection's scan. On GitHub the reserve holds both of the connection's clients, which share one budget. forge-scout logs `scan stopped` with `reason` `read_reserve`, the `phase` it stopped in, `budget_remaining` and `budget_reset`. `phase` is the step under way: `open`, `discovery`, `open pull requests`, `open issues`, `runs`, `pr checks`, `security`, `workflows` or `run state`, the save. It logs no snapshot and no `scan complete` for that connection. `scan stopped` carries every `_read` field and `unsupported_signals`, with `unread` for the signals the scan did not reach. Once `budget_reset` has passed, the next scan reads again on the same connection. The first stop in a budget window logs at `warn` and later ones at `info`, so each one still counts for the stall alert. A stop before the forge reported any budget logs at `warn` every time. A forge that refuses requests logs `scan stopped` with `reason` `rate_limited` and a `scan degraded` line.

Each attempt of a retried GitHub request counts against the reserve. A request that started above the reserve can end below it after a retry, and then the next one waits.

`scan stopped` also carries `scan_id`, `degraded`, `failed_signals` and `scan_interval_s`. Its `degraded` is `false` for a stop at the budget reserve after reads that all answered whole, and `true` for any other stop. Its `failed_signals` names the signals a read failed on before the stop, as on `scan complete`, and is empty when none did. A read the stop cut short is not named.

A connection's reads stop a tenth of its limit before it, one minute at most, so its save still fits inside the limit. A connection still reading then, or still saving at twice `scan_interval`, logs `scan stopped` at `warn` with `reason` `scan_timeout`, the `phase` it stopped in, its `limit` and a `hint`, then a `scan degraded` line. It logs no snapshot and no `scan complete` for that scan. The runs it read before the limit stay logged. A save cut off at the limit also logs `run state save failed`, and the next save that succeeds stores its runs. A save cut off while an earlier save is still writing logs no run lines. The next scan reads those runs again and logs them. The other connections finish and log as usual.

## Grafana dashboard

The dashboard needs Grafana 13.2 or newer and the container's log in Loki with a `container` label that holds the container name, which the Alloy config in the [monitoring guide](https://github.com/cplieger/docs/blob/main/docs/monitoring.md#the-smallest-stack-sends-notifications-only) sets. [Importing an app's dashboard](https://github.com/cplieger/docs/blob/main/docs/monitoring.md#importing-an-apps-dashboard) shows how to load [`grafana-dashboard.json`](../grafana-dashboard.json). The dashboard reads from your default Loki data source and needs no plugin. Its tabs follow the order you ask questions:

1. Overview holds the count tiles, with failing workflows first and a Scan integrity tile, above the failing workflows, the idlest open pull requests and the most severe code-scanning alerts.
2. CI runs holds the failed runs, the runs created per day by result and the failed runs per day by repository, the runs that passed after a re-run, the average run time, the slowest workflows and the workflows GitHub disabled.
3. Open work holds the open pull requests and issues over time with a table of each, idlest first, then the open work by repository and by age.
4. Security alerts holds the open alerts by severity, over time and by tool, the alert table and the coverage per connection.
5. Scout health holds the last scan per connection, what each scan read and what each connection cannot read, the degraded scans in the time range by connection and cause, each scan problem, the stopped scans, the runs coverage gaps, the remaining budget, the scan length and the failed reads per day by signal of finished and stopped scans. A scan that failed two signals counts once under each.

A **Forge** filter narrows every panel to one or more forges, and **Split by forge** shows one series per forge in the charts that add up connections. Charts drawn per connection or per repository stay as they are. The tabs are by topic, not by forge, so choosing one forge in the filter gives the view of that forge. All also shows a connection whose forge forge-scout could not detect, because it failed to open first. Its lines carry `forge` `unknown`, so it shows only under All, and its count tiles read "No whole count in the newest scan".

The Security alerts tab and the Overview's row of code-scanning alerts show only while a selected connection read code scanning in the time range, so they stay out of the way on GitLab, Gitea and Forgejo. A hidden variable asks Loki for the forges whose `scan complete` or `scan stopped` lines carry a `security_alerts_read` other than `unsupported` or `excluded`. When Loki cannot answer that question, the tab and the row stay shown.

The table of workflows GitHub disabled shows only while it has a row. The count tiles never hide. A tile that reads "Not on these forges, or no scan" has no selected connection with the signal, or none that finished a scan in the time range.

The count tiles add up each connection's newest `scan complete` or `scan stopped` line in the time range. A tile counts only a line whose read of the signal is `complete`, so it never shows a short count or an older scan's count. Any other read state makes the tile read "No whole count in the newest scan". So does a stop, because a `scan stopped` line logs no counts, even after reads that were whole. A forge without the signal adds nothing, and narrowing the **Forge** filter shows the other forges.

Each table reads one scan, by joining the newest scan's `scan_id` with the lines that carry it. The failed runs table reads the newest 500 `ci run` lines in the time range and keeps each run's newest line. The table of runs that passed after a re-run keeps one row per run and update time, so a line logged again after a crash shows once.

When more than one connection is in scope, every table of items shows each repository as `<repo> on <connection>`. So a repository that two connections both hold shows as two rows that say which is which. A hidden variable asks Loki for the connections of the selected forges that logged a scan in the time range. When it names only one, the tables show the repository alone. A long name wraps in its cell rather than being cut.

The charts of open pull requests, open issues, open work by age and code-scanning alerts over time take each connection's newest scan in each interval that read the signal whole. An interval is an hour unless the range spans more than a few weeks. So an item that closes, or ages into an older band, moves at the first interval whose newest scan saw the change.

A connection whose scans in an interval did not read the signal whole leaves a gap there, never a short count. That happens when a read failed or was cut short, the connection could not be opened or the scan stopped before logging counts. With Split by forge off, one connection's gap blanks the whole stacked total, so narrow the Forge filter or turn on Split by forge to see the other forges. A dip means a connection logged no scan.

The run charts read the `ci day` lines, never the `ci run` lines, which Loki dates by when it received them. Each bar stands on the UTC day the runs were created and shows each repository's newest `ci day` line for that day. So a run counts once however often a scan logged it, and a run a re-run fixed moves from failed to passed. A day some list did not cover counts at least the runs shown. The tooltip of Runs created per day by result shows how many repositories forge-scout read only in part that day.

The average run time per day divides the day's run time by its timed runs. A run chart draws only the days whose UTC midnight falls inside the time range, so it leaves out the day the range starts in, which the time axis would cut.

The stopped scans per day and the failed reads per day on Scout health count each scan on the UTC day its line was logged, and draw the days the same way as the run charts. So today's bar shows while the day is under way.

Failed runs per day by repository shows one series per repository and connection, so a repository that two forges both hold shows once on each. A range with more such series than Loki's `max_query_series` limit, 500 by default, shows an error on that chart. Raise the limit in Loki, or narrow the time range or the **Forge** filter.

Scan integrity counts the selected connections whose newest scan in the time range is degraded, from the `degraded` field of that `scan complete` or `scan stopped` line. So it turns green again at a connection's first clean scan, and a stop at the budget reserve after reads that all answered whole leaves it green. It reads "No scan in this range" when no selected connection finished or stopped a scan in the time range, rather than Complete. Degraded scans in this range, on Scout health, keeps the history by counting the scans of each connection that logged each cause.

Scout status reports whether the scan loop runs, so it reads every connection whatever the **Forge** filter. It reads Running while a connection's newest `scan complete` or `scan stopped` line is younger than 3.5 times the `scan_interval_s` that line carries, at the end of the time range. Two scans can end up to 3.1 intervals apart, so a slow scan still reads Running, and a new `scan_interval` needs no dashboard change. A time range shorter than the gap between two scans can hold no scan, and then the tiles and Scout status read "No scan in this range".

Every panel uses one Loki selector and names the fields it reads, for example:

```logql
{container="forge-scout"} |= `scan complete` | json msg="msg", forge="forge", connection="connection" | msg=`scan complete`
```

## Pinning the dashboard

The dashboard is versioned with the app. The JSON at release `<tag>` matches the log fields that image writes. The file sets `metadata.name` to `forge-scout`, which Grafana uses as the dashboard UID. Because the name stays the same from release to release, importing a newer copy over the existing one, or a file provider reading the newer file, updates it in place. Pin it the way you pin the image, to the tag of the image you run. Each release publishes it in two forms:

- The release asset `https://github.com/cplieger/github-scout/releases/download/<tag>/grafana-dashboard.json`, with `grafana-dashboard.json.sha256` beside it. It works as the Grafana Helm chart's `dashboards.<provider>.<name>.url` with `curlOptions: "-sLf"`, because the release URL redirects, or as a Terraform `http` data source. Renovate can track the URL with its `github-releases` datasource.
- The OCI artifact `ghcr.io/cplieger/github-scout/dashboard:<tag>`, with the artifact type `application/vnd.grafana.dashboard.v2+json`.

The file is a Grafana dashboard resource with `apiVersion: dashboard.grafana.app/v2`, which grafana-operator's `GrafanaDashboard` does not accept. Load it with a `GrafanaManifest`, as grafana-operator's [dashboards v2 example](https://grafana.github.io/grafana-operator/docs/examples/manifests/dashboards-v2/) shows. A `GrafanaManifest` takes the dashboard inline and has no URL or OCI source. Copy the `spec` object of the release's `grafana-dashboard.json` in place of the comment below, and copy it again when you move to a new tag.

```yaml
apiVersion: grafana.integreatly.org/v1beta1
kind: GrafanaManifest
metadata:
  name: forge-scout
spec:
  instanceSelector:
    matchLabels:
      dashboards: grafana
  template:
    apiVersion: dashboard.grafana.app/v2
    kind: Dashboard
    metadata:
      name: forge-scout
    spec:
      # The spec object of grafana-dashboard.json, unchanged.
```

For a Grafana instance the operator does not manage, also set `namespace` under `template.metadata` to that instance's namespace. You can leave it out when the `Grafana` resource sets `tenantNamespace` in `spec.external`.

If a `GrafanaDashboard` already loads this dashboard from an older tag, keep it on that tag, including in any Renovate rule that bumps it. On grafana-operator v5.25.0, a `GrafanaDashboard` that receives a file in this format deletes the dashboard it manages. The issue is [grafana/grafana-operator#2955](https://github.com/grafana/grafana-operator/issues/2955). Replace it with the `GrafanaManifest` above.

With Terraform, the Grafana provider's [`grafana_apps_dashboard_dashboard_v2`](https://registry.terraform.io/providers/grafana/grafana/latest/docs/resources/apps_dashboard_dashboard_v2) resource takes the file's `spec` object as JSON, and the dashboard's name as `uid`:

```hcl
data "http" "forge_scout_dashboard" {
  url = "https://github.com/cplieger/github-scout/releases/download/<tag>/grafana-dashboard.json"
}

resource "grafana_apps_dashboard_dashboard_v2" "forge_scout" {
  metadata {
    uid = "forge-scout"
  }
  spec {
    json = jsonencode(jsondecode(data.http.forge_scout_dashboard.response_body).spec)
  }
}
```

## Alerting

forge-scout has no metrics endpoint, so its state is in its logs. These rules are for Loki's ruler. Save the block below as a file in Loki's rules folder, as [Loading an app's alert rules](https://github.com/cplieger/docs/blob/main/docs/monitoring.md#loading-an-apps-alert-rules) shows. The first rule catches a connection whose scans ran but went blind. The second fires when no scan completes or stops at all.

```yaml
groups:
  - name: forge-scout
    rules:
      - alert: ForgeScoutScanDegraded
        expr: |
          sum by (connection) (count_over_time(
            {container="forge-scout"} |= `scan degraded`
            | json msg, connection | msg=`scan degraded` [100m]
          )) >= 2
        for: 0m
        labels:
          severity: warning
        annotations:
          summary: "forge-scout scans of {{ $labels.connection }} are degraded"
          description: >
            Repeated degraded scans of this connection in the last 100m. A
            count it could not read whole is withheld or marked by its _read
            field, never shown as 0. Check the cause and failed_signals fields
            of the scan degraded line.
      - alert: ForgeScoutScanStalled
        expr: |
          absent_over_time({container="forge-scout"} |= "scan "
            | json msg="msg" | msg="scan complete" or msg="scan stopped" [55m])
        for: 0m
        labels:
          severity: warning
        annotations:
          summary: "forge-scout has not finished a scan in 55m"
          description: >
            No scan complete or scan stopped line in 55m, so every panel is
            stale. Check that the container runs, that its logs reach Loki, and
            that the scan loop is alive.
```

Notes on each rule:

- `ForgeScoutScanDegraded` means scans of that connection keep failing to read a signal whole or to save the run state. The count tiles read "No whole count in the newest scan" for a count not read whole, and other connections keep their own counts. The `cause` values are in the table under [Scan summary and integrity](#scan-summary-and-integrity). A connection that keeps running past its limit logs `cause` `scan_timeout` on every scan, so this rule catches it. A run state that cannot be saved logs `cause` `state_unwritable` on every connection, and a `/data` whose folder cannot be synced logs `state_not_durable`.
- `ForgeScoutScanStalled` counts a stopped scan as finished, because a scan held back by the budget reserve is working as intended. It fires when the scan loop stops, the container stops or is renamed, or the log pipeline stops shipping.

Thresholds and the `severity` label are starting points. Both windows assume the default `scan_interval` of 15m. Two scans can end up to 3.1 intervals apart: the interval plus its 10% jitter, plus a scan at its limit of twice the interval.

- The stall rule's 55m is a little over 3.5 intervals, so even the widest gap between two scans holds one.
- The degraded rule needs two degraded scans in its window. Its 100m is more than 6.2 intervals, two of those gaps, so it keeps firing while every scan stays degraded. It resolves once fewer than two degraded scans fall in the last 100m.

Widen both windows in proportion if you lengthen the interval. Change the `container` selector to the label your log collector sets, such as `job` or `service`, and route by whatever labels your Alertmanager uses.

In a container the scan always runs in the main process, so these rules apply to every container deployment as written. If you run one-shot `trigger` scans from cron on a host, the lines land in your scheduler's output and not in a `forge-scout` container stream. Point the selectors there, and alert on the job's exit code for failed scans.

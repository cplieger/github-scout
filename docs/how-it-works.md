# How forge-scout works

This page explains what one scan does, why forge-scout writes logs and not metrics, how it logs each signal, the state it keeps and how much of each forge's budget it uses. It is for operators who want the mechanism behind the dashboard.

## One scan

The first scan runs as soon as the container starts, then one every `scan_interval`, give or take 10%. A scan reads every connection at the same time, and for each one:

1. Opens the connection, which detects the product and reads the account the token belongs to.
2. Lists the repositories of each owner and keeps those that are neither archived nor excluded.
3. Lists the open pull requests and issues of each owner, and keeps those in a kept repository whose author and labels are not excluded.
4. Reads each kept repository's CI runs created inside the lookback window, the last `lookback`.
5. Reads the checks of each open pull request's head commit, where the forge names one.
6. On GitHub, reads each repository's open code-scanning alerts and its workflow definitions, which give the disabled workflows and the ones deleted or renamed.

When a connection's reads end, forge-scout logs a `ci run` line for each run that is new or whose state or update time changed since a scan last saw it, then saves the runs it read. Delivery is at least once: a crash can repeat a run line, never drop one. A crash between the lines and the save logs them again after the restart, because the saved state does not hold them yet. When every connection has been read, forge-scout logs the open work, the failing and slowest workflows and the alerts of the whole scan in one burst, then one `scan complete` line per connection. Every line of the burst carries the same `scan_id`, the time the scan started in Unix milliseconds, and its `rank` in its list, so a dashboard reads exactly one scan. A scan stopped by the container shutting down still logs and saves its runs for 5 more seconds, so it ends inside Docker's default 10 second stop timeout. A run whose line those 5 seconds cut is not saved either, so the next start logs it. A shutdown before the burst logs none of it, and one during the burst ends it there, with no `scan complete` after that point, so a `scan_id` with no `scan complete` is not a whole scan.

Each connection's scan, its save included, may run for at most twice `scan_interval`, 30 minutes at the default. Its reads stop one minute earlier, or a tenth of the limit when that is shorter, to leave time for the save. A connection still reading then, or still saving at the limit, stops, logs `scan stopped` with `reason` `scan_timeout` and a `scan degraded` line, and publishes no open work, workflows or alerts for that scan. The runs it already read stay logged. The other connections are not held back.

A failure on one connection never touches another. A rejected token on GitLab marks only that connection degraded, and the GitHub counts stay as they are.

## Logs, not metrics

A run, an open pull request or an alert is an event with a title, an author and a link you want to click. A Prometheus counter would keep how many there are and lose everything you act on. So forge-scout writes structured logs, the dashboard counts them with LogQL, and every row keeps its repository, title and link.

The logs are JSON on standard output. Timestamps are UTC whatever the container's `TZ`.

## Two ways a signal is logged

- CI runs are events. A finished run is logged as a `ci run` line when it is first seen, and again each time its state or update time changes, such as after a re-run. A line logged again carries `previous_state`, which equals `state` when a re-run ended with the same result. A re-run keeps its run's creation time, so it is seen while its run is younger than `lookback`. A run ID is unique only within its repository, so a count of distinct `connection`, `repo` and `run_id` values equals the number of distinct runs.
- Open pull requests, open issues, alerts, the workflow lists and the listed runs counted per day are snapshots. The whole set is logged again on every scan, a closed item stops appearing, and the dashboard reads the latest scan as what is open now. Its run charts read the per-day counts of the last 8 days, which the run state keeps whatever `lookback` is, so each run stands on the day it was created in its newest state.

## State

forge-scout keeps one file, `/data/state.json`, beside a lock file. It holds, for each workflow on each branch, the result of its newest finished run there and how many runs in a row failed. It holds the state and update time of the last `ci run` line of each run created in the last `lookback`, and the newest state of each run created in the last 8 UTC days. It also holds when each repository's runs were last listed, with the creation time of the oldest run that list read still in progress. A run a pull request started never counts as a run of its branch, even on a branch with the same name as the default. The failing and slowest workflows read the default branch only.

One forge-scout process uses `/data` at a time. It locks the directory at start and holds the lock until it exits, so a second process given the same directory, such as a `trigger` run beside the daemon or a second container on the same volume, logs `state directory is in use by another forge-scout process` and exits 1 before it reads anything. A `/data` forge-scout cannot lock, such as one it cannot write, also stops it at start: it logs `run state directory unusable` at `error` level with the `path` and the `error`, and exits 1. forge-scout keeps the state in memory and replaces the whole file after each connection's scan.

- A run's last line is dropped at a save once the run was created before the lookback window, because no listing returns it again.
- Each listing judges the finished runs it read, oldest first. A run newer than its workflow's result on its branch becomes that result. A run read again in another state, such as after a re-run, updates the result it is. A run still in progress, or in a state forge-scout cannot map, is not judged or logged, and each scan reads it again while it is younger than `lookback`. An older run, such as a re-run of a run that is no longer the newest, changes its own `ci run` line and not the result.
- A workflow with no run on a branch for 63 days is dropped for that branch, so a monthly workflow stays known across one missed month. On GitHub, a workflow a whole workflows list shows as deleted leaves the failing list at once, and so does one an earlier whole list named and the latest no longer names. A workflow whose runs never carried a name the list holds, as with GitHub's own code scanning, Pages and Dependabot workflows, stays until the 63 days pass, as a deleted or renamed workflow does on the other forges, which list no workflows.
- Each scan's window must reach back past the start of the scan before it, which `lookback` of at least two `scan_interval`, each up to 10% late, plus one scan at its limit makes sure of. When it does not, as after the container was stopped for longer than `lookback`, the runs created between the two scans are never listed or logged. forge-scout logs `runs coverage gap` once for each repository, and the failure streak that follows carries `failing_since_clipped` `true`. A list cut short covers only its own runs, so a streak across the cut is clipped too, and a workflow whose result is older than the list's oldest run reads `unknown`. A list cut short of the scan before it leaves the runs between the two unlisted. A later scan whose window reaches back past them lists them, and otherwise forge-scout logs `runs coverage gap` for them once.
- When a repository's default branch changes, the next scan reads the workflows of the new default branch from what is already stored for it, so its failing and slowest workflows show at once. A workflow whose newest run on the old branch failed is no longer listed as failing.
- A repository that a complete listing no longer returns is forgotten, and its workflows leave the failing and slowest lists. Its runs' last lines are still kept until they leave the lookback window, so if the repository comes back, they are not logged again. A listing cut short, or one where an owner did not resolve, forgets nothing. An owner that resolves and answers no repository is listed whole, so its repositories are forgotten.
- When a save fails, forge-scout logs `run state save failed` at `error` level with the `path` and the `error`, and that connection's scan is degraded with `cause` `state_unwritable`. The runs it read still count toward the failing workflows. Their `ci run` lines are logged as usual, before the save, and the next save that succeeds stores those runs. If the process stops before then, the next start reads those runs again and logs them again.
- When a save writes the file but the file system cannot sync its folder, forge-scout logs `run state not durable` at `error` level with the `path` and the `error`, and that connection's scan is degraded with `cause` `state_not_durable`. The file holds the runs, so their `ci run` lines are logged at once. A power loss before the next durable save can undo the write, and the next start then logs those runs a second time.

The file survives a restart, so a restart does not log the lookback window again. A missing file is a first start. A file forge-scout cannot read, anything at that name that is not a regular file, one over 128 MiB, one written by an earlier version of the format, or one holding a run or streak forge-scout would never write is renamed to `state.json.corrupt-` followed by the time in Unix seconds. forge-scout logs `run state unreadable; starting cold` at `warn` level with its `path`, the `set_aside` path and the `error`, and the first scan logs every run inside the lookback window again. When the rename fails, the same line is logged at `error` level with `set_aside_error`. A workflow's failure streak then counts from its first run inside the window, and its line says so with `failing_since_clipped`. A run whose ID, state or times the file could not hold, such as an update time before its creation, fails its repository's runs read for that scan. forge-scout logs `runs listing failed` with the reason and stores none of that repository's runs, so `failing_workflows_read` reads `partial`.

## API use

forge-scout reads every forge through [forgeapi](https://github.com/cplieger/forgeapi), which pages through results, retries a failed request and reads the budget each response reports. Each scan makes, per connection:

- one listing request per page of repositories, per owner,
- one request per page of open pull requests and of open issues, per owner, or per project for a GitLab user,
- one run request per page per repository,
- on GitHub, one code-scanning and one workflows request per repository,
- one checks request per open pull request on GitHub and GitLab.

On Gitea and Forgejo the run listing has no time filter. forgeapi drops each page's runs created before the listing's start, and ends the listing at the first page whose runs were all created more than a minute before it, so a long run history costs one or two requests per repository. GitHub and GitLab filter by creation time on the server. GitHub cuts a list at 1,000 runs, and forge-scout stops one at 40 pages.

forge-scout holds back 250 requests of the budget each forge's last response reported. A read that would go below it is not made, the connection's scan stops and `scan stopped` is logged with the budget and its reset time. Once that reset time has passed, the next scan reads again. GitHub keeps separate budgets for its REST and GraphQL APIs, and a response reports only the one it drew on. On GitHub one account of the REST budget sees every response of the connection, from forgeapi and from the code-scanning and workflow reads, and holds every read of both at the reserve. forgeapi holds its GraphQL reads against the GraphQL budget itself. When the REST budget is at the reserve, the next scans read nothing on that connection until its reset time has passed. Gitea reports no budget, so the reserve has nothing to hold there.

A GitHub answer of 403 with no rate-limit header is a missing permission or a secondary rate limit, and GitHub asks a client to stop on a rate limit, so the connection sends no further request for the rest of the scan, through either client. The next scan reads again.

## Limits

- Code-scanning alerts and disabled workflows are read on GitHub only. GitLab's vulnerability reports are a different, paid feature and are not read.
- Dependabot alerts are left out, because Dependabot has its own alerting.
- A run still in progress, or waiting for a maintainer to approve a fork's run, is logged once a scan reads it finished while it is younger than `lookback`. A run that first finishes after it is older than `lookback`, as when it ran long or forge-scout was stopped meanwhile, is never logged, and its workflow keeps the result of its last finished run.
- A list cut short reads only the runs created from its oldest row on. A run the scan before read still in progress, created before that row, may finish without any list reading it again, and is then never logged. forge-scout logs `runs coverage gap` once for that repository, with `pending_since` the creation time of the oldest such run. On a repository busier than one list holds, whose runs often take longer than the span its list reaches back, this can repeat from scan to scan.
- Runs created while forge-scout was stopped for longer than `lookback` are never logged. `runs coverage gap` reports each such gap once.
- A re-run of a run older than `lookback` is not read, so the day charts keep that run's earlier result.

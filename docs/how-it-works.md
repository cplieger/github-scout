# How github-scout works

This page explains what one scan does, why github-scout writes logs and not metrics, how it logs each signal, the state it keeps and how much of GitHub's rate limit it uses. It is for operators who want the mechanism behind the dashboard.

## One scan

The first scan runs as soon as the container starts, then one every `SCAN_INTERVAL`, give or take 10%. Each scan does four things in order:

1. Lists the repositories owned by the token's account, keeps those whose owner is `GITHUB_OWNER` and drops archived ones.
2. Runs one search across those repositories for open pull requests and one for open issues.
3. For each repository not in `EXCLUDE_REPOS`, reads the finished Actions runs created inside the lookback window, then the open code-scanning alerts unless the repository is skipped for code scanning.
4. Logs one line per item, then a `scan complete` summary.

There is no list of repositories or workflows to keep. One request per repository returns every finished run of every workflow, so a new repository or workflow appears on the next scan.

When GitHub's search times out, it can answer with only part of the results. github-scout retries that answer instead of logging it, because a short list would read as pull requests or issues that closed.

## Logs, not metrics

A workflow run, an open pull request or an alert is an event with a title, an author and a link you want to click. A Prometheus counter would keep how many there are and lose everything you act on. So github-scout writes structured logs, the dashboard counts them with LogQL, and every row keeps its repository, title and link.

The logs are JSON on standard output. Timestamps are UTC whatever the container's `TZ`.

## Two ways a signal is logged

- Actions runs are logged once each. A finished run appears exactly once, as a `workflow run` line carrying its `conclusion`, so a count of lines equals the number of distinct runs.
- Open pull requests, open issues and code-scanning alerts are snapshots. The whole open set is logged again on every scan, a closed item stops appearing, and the dashboard reads the latest scan as what is open now.

## State

github-scout keeps no database, and Loki holds the history. Its local state is two disposable files under `/tmp`, beside the `/tmp/.healthy` health marker:

- `/tmp/seen-runs.json` holds the IDs of runs already logged, pruned to the lookback window. A `trigger` run shares the file, so a run is still logged once across one-shot scans.
- `/tmp/cond-cache.json` holds the validators GitHub returned for the repository list and for each code-scanning list, so an unchanged answer comes back as a 304 that costs no rate limit. Entries unused for 14 days are dropped.

Losing either file costs at most one scan that logs more lines or uses more of the rate limit. Three things start the run file cold:

- A container recreate clears `/tmp`. A plain restart keeps it with the shipped compose file.
- With the hardened profile in [Security](security.md), `/tmp` is a tmpfs, which Docker empties on every container start.
- The run file is read through a 64 KiB limit. On an account with thousands of runs inside the lookback window it outgrows that limit, and each start logs `dedup state corrupt; starting cold`.

After a cold start, the first scan logs every run inside the lookback window again. The dashboard's failed-run tile and table count distinct run IDs, so their numbers stay right.

## GitHub API use

A personal access token gets 5,000 REST requests an hour, and the search endpoints have a separate, smaller limit. Each scan makes these requests:

- one repository list request for every 100 repositories,
- one Actions request per repository, plus one for each further 100 finished runs inside the lookback window, up to 500 runs,
- one code-scanning request per repository that is not skipped,
- two searches, counted against the search limit.

The repository list and the code-scanning lists are conditional requests. When nothing changed, GitHub answers 304, which [does not count against the limit](https://docs.github.com/en/rest/using-the-rest-api/best-practices-for-using-the-rest-api#use-conditional-requests). So at the default 15 minutes, an account of 100 repositories with little activity uses about 400 requests an hour. The Actions request always counts, because its time window moves with every scan.

Failed requests are retried with backoff. A rate limit that outlasts the retries is reported as `rate_limited`, described in [Monitoring and alerts](monitoring.md#scan-summary-and-integrity).

## Limits

- github-scout works with github.com only. GitHub Enterprise Server would need a configurable API address, which it does not have.
- Dependabot alerts are left out, because Dependabot has its own alerting.
- A scan covers at most 500 repositories, 500 open pull requests, 500 open issues and 500 finished runs per repository.

# Contributing to forge-scout

The [shared contributing rules](https://github.com/cplieger/.github/blob/main/CONTRIBUTING.md) for commits, releases, synced files and checks apply here.

## Rules

Treat each `msg` string and field name logged in `internal/collect` as a shared contract. Rename it in every place that uses it, including `grafana-dashboard.json`, `docs/monitoring.md` and the README. The Go tests in `internal/collect/docs_test.go` check the cause table, the read-state fields and the `scan complete` fields of `docs/monitoring.md` against the code. The tests in `internal/collect/dashboard_contract_test.go` fail when a panel of `grafana-dashboard.json` reads a `msg` or field that the recorded scans do not log, maps fewer read states, causes or stop reasons than the code has, or counts `ci run` lines without the `connection`, `repo`, `run_id`, `state` and `updated_at` key. No test reads the README.

Every forge read goes through `internal/forgeconn`, the one package that holds forgeapi's role and row types. `internal/config` uses only forgeapi's owner validator and `main` only its pinned GitHub API version. A read forgeapi does not model belongs in `internal/githubrest` when it is GitHub's, and is reported as unsupported on the other forges, never as 0.

A new signal touches eight places:

1. A row type in `internal/forge`.
2. A read method on `forgeconn.Client`, or on `githubrest.Client` for a GitHub-only route, with a test against the `forgeconntest` fake of each product it reads.
3. The same method on the `Conn` or `GitHubReader` interface and on the fakes in `internal/collect/collect_test.go`.
4. A family in `internal/collect/ledger.go` and a read step in `internal/collect/conn.go` that records every read, drop and stop on the connection's `readLedger`, so a failed or partial read never logs as complete or as zero items.
5. Its lines in `internal/collect/emit.go`, ranked by `rank` for a snapshot, and its counts in `complete.go`, absent when the signal was not read.
6. A panel in `grafana-dashboard.json` that filters on the new `msg`, and a line of that `msg` in the scans `recordContract` runs in `internal/collect/dashboard_contract_test.go`.
7. A row in the log table of `docs/monitoring.md`, and the signal's name in its list of `_read` fields.
8. The signal in the README's `## What it does` list, and in `## What each forge supports` when a forge lacks it.

Save a `grafana-dashboard.json` change made in the Grafana UI with Export, then Export as code, choosing the V2 Resource model.

## Releases

Use a releasing type such as `fix:` for a commit that changes only `grafana-dashboard.json`. A non-releasing type makes the dashboard wait for the next [release](docs/monitoring.md#pinning-the-dashboard).

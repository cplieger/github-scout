# Contributing to github-scout

The [shared contributing rules](https://github.com/cplieger/.github/blob/main/CONTRIBUTING.md) for commits, releases, synced files and checks apply here.

## Rules

Treat each `msg` string and field name logged in `internal/collect` as a shared contract. Rename it in every place that uses it, including `internal/ghsignal`, `grafana-dashboard.json`, `docs/monitoring.md` and the README. The Go tests do not read the last three files, so a rename that updates the Go code can empty the panels while CI passes.

A new signal touches nine places:

1. A type in `internal/ghsignal` whose JSON tags match its log keys, added to the cases of `TestLogKeysMatchSignalTags`.
2. A read method on `internal/github.Client` that checks each path segment with `urlsafe.IsSafeURLSegment`, with an `httptest` test and a fuzz target for a new response shape.
3. The same method on the `apiClient` interface and on `fakeClient` in `internal/collect`.
4. A `collect<Signal>` method that logs once per item or as a snapshot, as [Two ways a signal is logged](docs/how-it-works.md#two-ways-a-signal-is-logged) describes.
5. A `record` method on `scanIntegrity` that each read reports to, with its counters added to `errCount`, `failedSignals`, `anyReadSucceeded` and `escalate`. Without them, a failed read logs zero items on a scan reported clean.
6. For a snapshot signal, a count field on the `scan complete` line in `Scan`, which the dashboard's count tiles read.
7. A panel in `grafana-dashboard.json` that filters on the new `msg`.
8. A row in the log table of `docs/monitoring.md`, and the signal's name in its `failed_signals` list.
9. The signal in the README's `## What it does` list and `## Monitoring` paragraph.

Save a `grafana-dashboard.json` change made in the Grafana UI with Export, then Export as code, choosing the V2 Resource model.

## Releases

Use a releasing type such as `fix:` for a commit that changes only `grafana-dashboard.json`. A non-releasing type makes the dashboard wait for the next [release](docs/monitoring.md#pinning-the-dashboard).

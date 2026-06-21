# Contributing to github-scout

Notes on the architecture, local workflow, and conventions specific to this
repo. The org-wide `cplieger` defaults still apply; this file adds the
code-grounded detail you need to land a change without tripping over the
load-bearing patterns.

> This repository is **homelab-grade** software (see the disclaimer in the
> README). Contributions are welcome, but the maintainer optimises for a small,
> auditable, single-purpose tool over breadth of features. Please open an issue
> to discuss anything larger than a bug fix before writing code.

## What github-scout is (and isn't)

It does one thing: scan a GitHub owner's repositories and emit each newly-failed
Actions run as a structured log line for Loki. It is deliberately **not** a
general GitHub metrics exporter — pull requests, issues, code-scanning, and
Dependabot are out of scope for v1 because the Grafana GitHub datasource already
covers them. Keep that focus in mind when proposing changes; "surface an
actionable event" fits, "expose another number on a dashboard" usually doesn't.

The design rationale (logs over metrics, event-once dedup, stateless) lives in
the [README](README.md#design). Changes that contradict those decisions need a
strong justification.

## Architecture

`github-scout` is a single Go binary that polls the GitHub REST API on a
schedule and emits failed-run events as JSON to stdout. It is stateless — no
database, no on-disk state beyond the health marker; history lives in Loki.
There is no HTTP server and no listening port.

`main.go` is a **pure composition root** — it wires config → `httpx` client →
`github.Client` → `collect.Collector` → health marker, then runs the
signal-driven poll loop. It contains no business logic; everything testable
lives under `internal/`:

- `internal/config` — env-var loading, clamping, and validation (`Load`).
- `internal/github` — the GitHub REST client: `ListRepos` (via
  `/user/repos?affiliation=owner`, so private repos are included) and
  `ListFailedRuns` (one paginated query per failure conclusion). Auth headers,
  body caps, and page-count pagination live here.
- `internal/collect` — the scan orchestrator: discover repos → list failed runs
  → dedup by run ID → emit. It depends on a small consumer-side `apiClient`
  **interface** (not the concrete client), which is the seam that lets the
  orchestration logic be unit-tested with a scripted fake while the HTTP client
  is tested separately against an `httptest` server.
- `internal/model` — domain types (`Repo`, `FailedRun`). Their JSON tags are the
  Loki field names the dashboard queries, so treat them as a contract.
- `internal/urlsafe` — URL path-segment validation, applied to every owner/repo
  name before it is interpolated into a request URL.

Dependencies flow one direction: concrete packages depend on `model` /
`urlsafe`, `collect` depends on its own `apiClient` interface, and `main.go` is
the only place that wires the concrete client into the collector.

## Development environment

You need Go — the exact version is pinned in [`go.mod`](go.mod). No other
runtime tooling is required to build or test.

```bash
git clone https://github.com/cplieger/github-scout
cd github-scout
go build ./...
```

The lint and scan tooling is shared across the `cplieger` fleet and pinned by
[`cplieger/ci`](https://github.com/cplieger/ci). To install the exact versions
CI uses (golangci-lint, gitleaks, govulncheck, fieldalignment, …) so your local
results match the gate, run that repo's `scripts/install-local-tools.sh`.

Several config files (`.golangci.yaml`, `.editorconfig`, `LICENSE`, the CI
workflows, `renovate.json`, `cliff.toml`) are **not** committed here — they are
synced in from `cplieger/ci` and marked `DO NOT EDIT`. Change CI behaviour
upstream, not in this repo; a local copy would be overwritten by the next sync.

## Running it locally

Use one-shot mode (`POLL_INTERVAL_MINUTES=0`) with a token so you scan once and
exit instead of looping:

```bash
GITHUB_TOKEN=ghp_xxx \
GITHUB_OWNER=your-login \
POLL_INTERVAL_MINUTES=0 \
LOG_LEVEL=debug \
go run .
```

A read-only fine-grained token (Contents: read + Actions: read) is enough. You
will see the structured JSON events on stdout exactly as Loki would receive
them. The full env-var reference is in the README's configuration table.

## Running checks

```bash
go test ./...                 # unit + table-driven + fake-driven tests
go test -race ./...           # concurrency paths (CI runs this)
go test -cover ./...          # coverage
golangci-lint run ./...       # lint (also flags unformatted files)
golangci-lint fmt ./...       # apply gofumpt + gci formatting
```

Conventions for tests:

- **Table-driven** for pure logic (config parsing/clamping, model helpers).
- The **HTTP client** (`internal/github`) is tested against an `httptest`
  server — assert on the request (auth header, query params, pagination) and the
  decoded result. Never hit the real GitHub API in a test.
- The **orchestrator** (`internal/collect`) is tested with a scripted fake that
  satisfies the `apiClient` interface, so dedup / pruning / health semantics run
  without any network.
- Anything that **parses untrusted input** gets a fuzz target. The API JSON
  decode has `FuzzDecodeRunsPage`; keep it green and extend it if you add a
  parser:

  ```bash
  go test ./internal/github -run=x -fuzz=FuzzDecodeRunsPage -fuzztime=30s
  ```

A few house rules the linters enforce that are easy to trip on:

- **`sloglint` kv-only** — plain key/value pairs in `slog` calls, not attribute
  constructors.
- **`fieldalignment`** — order struct fields to minimise padding (pointer- and
  `time.Time`-bearing fields placed to keep the GC scan range tight).
- **No new non-`cplieger` runtime dependencies** without discussion — the small,
  auditable supply chain is a feature.
- **URL segments from input** must go through `internal/urlsafe`.

## The extension point: adding a new signal type

github-scout is structured so a new actionable signal (say, "a deployment was
left pending") can be added without disturbing the failed-run path:

1. **Model** — add a type in `internal/model` with JSON tags that become the
   Loki field names (a contract the dashboard queries).
2. **Client** — add a read method on `internal/github.Client` (page-count
   pagination via `getJSON`, `urlsafe` for any path segments).
3. **Interface** — extend the consumer-side `apiClient` interface in
   `internal/collect` so the fake can script it.
4. **Collector** — emit the event with its own stable `msg` string and dedup
   key, mirroring `emit` / `markSeen` / `prune`.
5. **Dashboard** — add a panel to `grafana-dashboard.json` filtering on the new
   `msg`.
6. **Tests** — `httptest` coverage for the client, fake-driven coverage for the
   collector, and a fuzz target if you parse a new response shape.

Keep each signal **actionable** and **event-shaped**. If what you want is a
trend line of a number, it belongs in a Prometheus exporter, not here.

## Commits and pull requests

Branch from `main`, keep changes focused with tests, and open a PR. Commit
messages follow [Conventional Commits](https://www.conventionalcommits.org/) —
git-cliff parses them to build release notes and pick the version bump
(`feat:` → minor, `fix:` / `sec:` → patch/security, `feat!:` → major; `chore`,
`ci`, `docs`, `test`, etc. don't release). Write the subject as the changelog
line a user would read. CI must be green: the required `ci / validate` check
builds the binary and Dockerfile and runs vet/lint/race-tests/govulncheck.
Releases are automated from the commit history on merge to `main` — contributors
don't tag or publish manually.

## Conduct and security

By participating you agree to the org-wide
[Code of Conduct](https://github.com/cplieger/.github/blob/main/CODE_OF_CONDUCT.md).
Report security vulnerabilities through the
[security policy](https://github.com/cplieger/.github/blob/main/SECURITY.md),
never in a public issue.

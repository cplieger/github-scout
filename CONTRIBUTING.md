# Contributing to github-scout

Notes on the architecture, local workflow, and conventions specific to this
repo. The org-wide `cplieger` defaults still apply; this file adds the
code-grounded detail you need to land a change without tripping over the
load-bearing patterns.

> This is a small, single-purpose self-hosted tool (see the disclaimer in the
> README). Contributions are welcome, but the maintainer optimises for a small,
> auditable, single-purpose tool over breadth of features. Please open an issue
> to discuss anything larger than a bug fix before writing code.

## What github-scout is (and isn't)

It does one thing: scan a GitHub owner's repositories and surface the items that
need a look (open pull requests, open issues, code-scanning alerts, and failed
Actions runs) as structured log lines for Loki. It is deliberately **not** a
general GitHub metrics exporter: Dependabot is out of scope (it has its own
alerting), and anything that is just "another number on a dashboard" doesn't
fit. Keep that focus in mind when proposing changes; "surface an actionable
item" fits, "expose a trend line" usually doesn't.

The design rationale (logs over metrics, the event-once vs snapshot emission
models, stateless) lives in the [README](README.md#design). Changes that
contradict those decisions need a strong justification.

## Architecture

`github-scout` is a single Go binary that polls the GitHub REST API on a
schedule and emits four actionable signals as JSON to stdout. It keeps no
database; history lives in Loki. The only cross-scan state is the event-once
run dedup set, which lives in memory and is also persisted to a small
`/tmp/seen-runs.json` so it survives across one-shot `trigger` processes (see
the README's _State_ section). There is no HTTP server and no listening port.

`main.go` is a **pure composition root**: it wires config → `httpx` client →
`github.Client` → `collect.Collector` → health marker, then runs the
signal-driven poll loop. It contains no business logic; everything testable
lives under `internal/`:

- `internal/config`: env-var loading, clamping, and validation (`Load`).
- `internal/github`: the GitHub REST client: `ListRepos` (via
  `/user/repos?affiliation=owner`, so private repos are included),
  `ListRuns` (one paginated `status=completed` query covering all conclusions),
  `SearchOpenPRs` / `SearchOpenIssues` (one cross-repo `/search/issues` query
  each), and `ListCodeScanningAlerts` (per-repo; a 404 means no analyses and is
  skipped silently, while a 403 (Advanced Security off, missing token scope, or
  rate limit) is surfaced rather than silently read as zero alerts). Auth
  headers, body caps, and page-count pagination live here.
- `internal/collect`: the scan orchestrator: discover repos → collect the four
  signals → emit. It depends on a small consumer-side `apiClient` **interface**
  (not the concrete client), which is the seam that lets the orchestration logic
  be unit-tested with a scripted fake while the HTTP client is tested separately
  against an `httptest` server.
- `internal/model`: domain types (`Repo`, `WorkflowRun`, `PullRequest`, `Issue`,
  `CodeScanningAlert`). The structs are never JSON-marshaled on the emit path;
  the Loki field names are the literal slog keys emitted in `internal/collect`,
  and the JSON tags here mirror those keys for documentation. Renaming a tag
  does not change a Loki field; renaming a slog key does. `TestLogKeysMatchModelTags`
  fails the build if the two drift.
- `internal/urlsafe`: URL path-segment validation, applied to every owner/repo
  name before it is interpolated into a request URL.

Dependencies flow one direction: concrete packages depend on `model` /
`urlsafe`, `collect` depends on its own `apiClient` interface, and `main.go` is
the only place that wires the concrete client into the collector.

## Systemic-failure causes

A scan that runs but goes blind escalates to a distinct `error`-level
`msg="scan degraded"` line carrying a machine `cause`, a human `reason`, and
`failed_signals`. The `cause` enum:

- `token_invalid`: a 401 rejected the token **and** no signal could be read this
  scan (a genuinely dead or blocked token). A single transient 401 alongside a
  successful read is treated as a secondary-rate-limit blip, reported as
  `degraded` but not escalated, since GitHub returns intermittent 401s under
  burst even on a valid token.
- `rate_limited`: a 429 response.
- `no_repos_visible`: discovery succeeded but returned zero repositories (a token
  that lost repo visibility, so nothing was scanned).
- `code_scanning_blind` / `runs_blind`: a per-repo signal could not be read for
  any repo that has it (e.g. a missing token scope). A repo that simply lacks
  code scanning, or one listed in `CODE_SCANNING_EXCLUDE_REPOS`, is excluded, so
  it never masks a real blackout.
- `signal_blind`: a cross-repo search (PRs or issues) failed.

A per-repo failure (an incidental error, or one private repo without GitHub
Advanced Security returning 403 on code scanning) is `degraded`-only, never
escalated. The dashboard's Scan Integrity tile and the `GithubScoutScanDegraded`
Loki ruler alert key on the `scan degraded` line, so a couldn't-check `0` is
never read as a confirmed-clean `0`.

## Development environment

You need Go; the exact version is pinned in [`go.mod`](go.mod). No other
runtime tooling is required to build or test.

```bash
git clone https://github.com/cplieger/github-scout
cd github-scout
go build ./...
```

The lint and scan tooling is shared across the `cplieger` repos and pinned by
[`cplieger/ci`](https://github.com/cplieger/ci). To install the exact versions
CI uses (golangci-lint, gitleaks, govulncheck, fieldalignment, …) so your local
results match the gate, run that repo's `scripts/install-local-tools.sh`.

Several config files (`.golangci.yaml`, `.editorconfig`, `LICENSE`, the CI
workflows, `renovate.json`, `cliff.toml`) are **not** committed here; they are
synced in from `cplieger/ci` and marked `DO NOT EDIT`. Change CI behaviour
upstream, not in this repo; a local copy would be overwritten by the next sync.

## Running it locally

Use the `trigger` subcommand with a token so you scan once and exit instead of
looping (this is also how an external scheduler drives it; see the README's
"Run modes"):

```bash
GITHUB_TOKEN=ghp_xxx \
GITHUB_OWNER=your-login \
LOG_LEVEL=debug \
go run . trigger
```

A read-only fine-grained token works well: Repository access = All
repositories; Permissions (all read) = Actions, Pull requests, Issues, Code
scanning alerts (Metadata: read is automatic). You will see the structured JSON
events on stdout exactly as Loki would receive them. The full env-var reference
is in the README's configuration table.

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
  server: assert on the request (auth header, query params, pagination) and the
  decoded result. Never hit the real GitHub API in a test.
- The **orchestrator** (`internal/collect`) is tested with a scripted fake that
  satisfies the `apiClient` interface, so dedup / pruning / health semantics run
  without any network.
- Anything that **parses untrusted input** gets a fuzz target. The API JSON
  decodes have one each: `FuzzDecodeRunsPage`, `FuzzDecodeSearchResp`,
  `FuzzDecodeCodeAlerts`; keep them green and add one if you parse a new
  response shape:

  ```bash
  go test ./internal/github -run=x -fuzz=FuzzDecodeRunsPage -fuzztime=30s
  ```

A few house rules the linters enforce that are easy to trip on:

- **`sloglint` kv-only**: plain key/value pairs in `slog` calls, not attribute
  constructors.
- **Logs are UTC**: a `utcTimeAttr` slog `ReplaceAttr` forces every
  record's timestamp to UTC, so the image needs no `TZ` and embeds no
  `time/tzdata`.
- **`fieldalignment`**: order struct fields to minimise padding (pointer- and
  `time.Time`-bearing fields placed to keep the GC scan range tight).
- **No new non-`cplieger` runtime dependencies** without discussion; the small,
  auditable supply chain is a feature.
- **URL segments from input** must go through `internal/urlsafe`.

## The extension point: adding a new signal type

github-scout is structured so a new actionable signal (say, "a deployment was
left pending") can be added without disturbing the failed-run path:

1. **Model**: add a type in `internal/model`. Its JSON tags document the
   fields, but the Loki field names are the literal slog keys you emit in
   step 4 — keep the tags in sync with those keys (the four existing signals
   are guarded by `TestLogKeysMatchModelTags`; add your new type to it).
2. **Client**: add a read method on `internal/github.Client` (page-count
   pagination via `getJSON`, `urlsafe` for any path segments).
3. **Interface**: extend the consumer-side `apiClient` interface in
   `internal/collect` so the fake can script it.
4. **Collector**: add a `collect<Signal>` method that emits each item with its
   own stable `msg` string; choose the event-once model (dedup by ID, like
   `collectFailedRuns` + `prune`) or the snapshot model (re-emit the full
   current set each scan, like `collectPRs`).
5. **Dashboard**: add a panel to `grafana-dashboard.json` filtering on the new
   `msg`.
6. **Tests**: `httptest` coverage for the client, fake-driven coverage for the
   collector, and a fuzz target if you parse a new response shape.

Keep each signal **actionable** and **event-shaped**. If what you want is a
trend line of a number, it belongs in a Prometheus exporter, not here.

## Commits and pull requests

Branch from `main`, keep changes focused with tests, and open a PR. Commit
messages follow [Conventional Commits](https://www.conventionalcommits.org/);
git-cliff parses them to build release notes and pick the version bump
(`feat:` → minor, `fix:` / `sec:` → patch/security, `feat!:` → major; `chore`,
`ci`, `docs`, `test`, etc. don't release). Write the subject as the changelog
line a user would read. CI must be green: the required `ci / validate` check
builds the binary and Dockerfile and runs vet/lint/race-tests/govulncheck.
Releases are automated from the commit history on merge to `main`; contributors
don't tag or publish manually.

## Conduct and security

By participating you agree to the org-wide
[Code of Conduct](https://github.com/cplieger/.github/blob/main/CODE_OF_CONDUCT.md).
Report security vulnerabilities through the
[security policy](https://github.com/cplieger/.github/blob/main/SECURITY.md),
never in a public issue.

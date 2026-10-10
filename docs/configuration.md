# Configuration

This page covers the tokens for each forge, which repositories forge-scout scans, the settings that leave repositories and items out, how the file is read, and one-shot scans. The README's [configuration reference](../README.md#configuration-reference) lists every key with its default, and [`config.example.yaml`](../config.example.yaml) is a starting file.

## Token permissions

forge-scout only reads, so each token needs read access and nothing more. A token is named in the file as `${NAME}`, and the value comes from the container's environment. The `token` value must be that one reference and nothing else, such as `token: ${GITHUB_TOKEN}`. A token written into the file, or text before or after the reference, stops the start with an error that names the connection and never the value. The name must end in `_TOKEN` or start with `FORGE_SCOUT_`, so an unrelated variable is never read into the file. A variable a token reads, and any name ending in `_TOKEN`, may appear only in a `token` value, because every other value can reach a log line.

- On GitHub, a fine-grained token with **All repositories** and **Read-only** access to **Actions**, **Commit statuses**, **Pull requests**, **Issues** and **Code scanning alerts**. GitHub adds read-only **Metadata** by itself. A classic token needs the `repo` scope, or `public_repo` for public repositories only. Avoid **Only select repositories**, which freezes the set, so a repository you create later is never scanned. GitHub gives a fine-grained token no permission for check runs. Where GitHub then refuses a pull request checks read, the connection sends no further request that scan, so every scan of it is degraded. A classic token with `repo` avoids this, so use one for a GitHub connection that reads pull request checks.
- On GitLab, a personal or group access token with the `read_api` scope.
- On Gitea and Forgejo, an access token with read access to the repository, issue and user scopes.

## Which repositories it scans

Each connection scans the repositories its `owners` own. An owner is a user, an organization, or on GitLab a group by its full path, such as `group/subgroup`. A GitLab group's projects include its subgroups' projects. Private repositories are included when the token can see them.

Archived repositories reach no signal and need no entry in any list. A fork keeps every signal except code scanning, which `security.skip_forks` skips by default, because GitHub reports the alerts of the code a fork copied as the fork's own. A private repository also keeps every signal except code scanning, which `security.include_private` leaves off by default, because GitHub reads a private repository's code scanning only when the account has GitHub Advanced Security.

An owner the forge does not know is reported as `owner_unresolved`, and that owner's repositories are not read. On GitLab, a user who is not a group has no owner-wide pull request or issue list. forge-scout then reads each of that user's projects instead, which costs two requests per project per scan.

A scan reads at most 10 pages of repositories per owner, 40 pages of open pull requests or issues per owner, and 40 pages of runs per repository. A list cut short marks the scan degraded, and a cut pull request or issue list is left out whole, so a shorter list is never read as items that closed. A repository list cut short leaves out that scan's lines and counts for pull requests, issues, code scanning and disabled, failing and slowest workflows. Those are read through the repositories the list returned. Its `ci run` lines are still logged.

## Leaving repositories and items out

- `exclude_repos` drops a repository from every signal. Write the full path, such as `owner/name`. Matching ignores case.
- `exclude_authors` and `exclude_labels` leave out open pull requests and issues by those authors or with any of those labels. Bot logins differ per forge, so check what your forge reports: the Debug log names each item it leaves out, and `scan complete` counts them as `excluded_prs` and `excluded_issues`.
- `bot_authors` names logins that the dashboard's From column shows as bots. Any login ending in `[bot]` counts as a bot already.
- `security.include_private: true` reads code scanning on private repositories too. Set it when the account has GitHub Advanced Security on its private repositories. Their workflows are read either way.
- `security.skip_repos` skips only the code-scanning read of a repository and keeps its other signals. Use it for a repository that answers every code-scanning read with 403, such as a private one without GitHub Advanced Security while `include_private` is on.
- `security.enabled: false` turns code scanning off for the connection.

A skipped repository counts neither as read nor as failed, so it never marks a scan degraded and never hides a real code-scanning outage. A repository that is not skipped and answers 403 without a rate-limit header stops every request of that connection for the rest of the scan, because GitHub sends the same answer for a secondary rate limit, and the scan is degraded (see `github_refused` in [Monitoring](monitoring.md#scan-summary-and-integrity)). A `security` key on a connection that is not GitHub logs `security settings ignored: code scanning is GitHub-only` once per scan.

## How the file is read

forge-scout reads the file once when the container starts, from `/config/config.yaml` or the path in `CONFIG_PATH`. An unknown key, a second YAML document, a missing token or an `http://` address without `allow_plaintext: true` stops the start with `invalid configuration`, and the error names the key and the connection. It never prints a token.

A duration it cannot read falls back to its default with a warning, and a value out of range is moved to the nearest limit. `scan_interval` stays between 1 minute and 171 hours and `lookback` between 1 and 720 hours. Both are [Go durations](https://pkg.go.dev/time#ParseDuration), such as `90s`, `15m` or `2h`. Each scan starts up to 10% earlier or later than the interval. Each connection is read at the same time as the others, and its scan stops at twice `scan_interval`, so a connection that needs longer needs a longer interval. No value turns scanning off: `off` and `0` fall back to the default.

`lookback` must be at least two `scan_interval`, each up to 10% late, plus one scan at its limit of twice `scan_interval`, so 4.2 `scan_interval`, after both are read and moved into range. Every scan reads the CI runs created in the last `lookback`, so each scan's read reaches back past the start of the scan before it, even when one scan is late and the one before ran to its limit. A file that breaks this stops the start with an error naming both values and the smallest `lookback` that fits. A re-run keeps its run's creation time, so it is seen while its run is younger than `lookback`. Runs created while forge-scout was stopped for longer than `lookback` are never read, and `runs coverage gap` reports each such gap once, as [Monitoring](monitoring.md) explains.

GitHub cuts a run listing at 1,000 runs, and forge-scout stops one at 40 pages on any forge. A cut listing leaves the runs read `partial` and marks the scan degraded. A workflow whose last result is older than the listing's oldest run reads `unknown` on its `failing workflow` line for that scan.

Two connections to one forge may not use tokens of the same account, because both would spend that account's request budget without knowing of each other. Two `url` values name one forge when they differ only in the case of the scheme or host, a default port such as `:443`, or a trailing `/`. Every URL on `github.com`, `www.github.com` or `api.github.com`, with any path and `http` or `https`, names GitHub.com. When two connections share an account, the one listed later fails to open each scan with `connection "<b>" uses the same account as "<a>" on <url>; list both owners in one connection`, and its signals read nothing, with cause `connection_failed`. List all of that account's owners in one connection.

`private_addresses: true` lets a connection reach an instance on a private or loopback address, such as a Forgejo server on your network. Without it, forge-scout refuses to connect there.

## One-shot scans

`forge-scout trigger` runs one scan, writes its lines to its own output and exits. The exit code is 0 only when every connection read every signal its forge has whole, and 1 otherwise: for an invalid configuration, a connection that could not be opened or listed, a failed or cut read, a stop at the forge's rate limit or the request reserve, a connection that ran past its limit of twice `scan_interval`, or a run state that could not be saved or made durable. A `runs coverage gap` does not count against it, nor does a run in a state forge-scout cannot map, which [Monitoring](monitoring.md) describes. When `SIGINT` or `SIGTERM` stops the scan, it logs `trigger scan interrupted` and exits 1. Otherwise its last line is `trigger scan complete` with `outcome` `complete` or `incomplete`.

The trigger uses the run state in `/data`, so a run is still logged once across one-shot scans. It is for a standalone run, not for use beside the daemon: one process holds `/data` at a time, so a trigger started while the daemon runs logs `state directory is in use by another forge-scout process` and exits 1 without reading anything. A `/data` it cannot lock, such as one it cannot write, logs `run state directory unusable` and exits 1 the same way.

In a container, leave scheduling to the built-in timer. A scan started with `docker exec forge-scout /forge-scout trigger` writes to the exec session and not to the container log, so the dashboard and the alert rules never see it. A one-shot scan never touches the health marker.

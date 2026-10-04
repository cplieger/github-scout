# Configuration

This page covers the GitHub token, which repositories github-scout scans, the settings that leave repositories and items out, how values are read, and one-shot scans. The README's [configuration reference](../README.md#configuration-reference) lists every variable with its default.

## Token permissions

github-scout reads four signals, so the token needs read access to repository metadata, Actions, pull requests, issues and code scanning. Both token types work, and both pick up a repository you create later on the next scan.

- A fine-grained token is the one to choose. Set its repository access to **All repositories**, so repositories you create later are scanned too. Under repository permissions, set **Actions**, **Pull requests**, **Issues** and **Code scanning alerts** to **Read-only**. GitHub adds read-only **Metadata** by itself, and github-scout lists your repositories with it.
- A classic token needs the `repo` scope, which covers private and public repositories for all four signals, or `public_repo` for public repositories only. The `workflow` and `security_events` scopes are not needed, because `repo` already grants read access to Actions and code scanning.

Avoid **Only select repositories** on a fine-grained token. It freezes the set, so a repository you create later is never scanned.

## Which repositories it scans

One container scans one account. github-scout lists the repositories owned by the account the token belongs to, private ones included, and keeps those whose owner is `GITHUB_OWNER`. So set `GITHUB_OWNER` to the login of that account. Repositories that belong to an organization, and repositories where you are only a collaborator, are not scanned. To watch a second account, run a second container with that account's token.

Archived repositories are dropped when the repositories are listed, so they reach no signal and need no entry in any exclusion list.

A scan lists at most 500 repositories. Past that number, github-scout logs `repo listing hit pagination bound` and the rest are not scanned. Each search returns at most 500 open pull requests or issues, and logs `search hit pagination bound` when it reaches that limit.

## Leaving repositories and items out

- `EXCLUDE_REPOS` drops a repository from every list. Names are bare, so `my-repo` and not `owner/my-repo`, and they match whatever their case.
- `CODE_SCANNING_EXCLUDE_REPOS` skips only the code-scanning read of a repository and keeps its runs, pull requests and issues. Use it for a private repository on a plan without GitHub Advanced Security. GitHub answers every code-scanning read there with 403, so each scan logs `code scanning listing failed` and is marked degraded until you add the name.
- `CODE_SCANNING_EXCLUDE_FORKS`, on by default, applies the same skip to every fork. GitHub reports the alerts of the code a fork inherited as the fork's own, so a fork of a large project brings hundreds of findings in code you did not write. One fork of a large project measured 500 open alerts, against 6 across every first-party repository of the same account. The flag covers a new fork from its first scan, with no name to add. Set it to `false` if your forks carry enough of your own code to be worth scanning.

The fork flag and `CODE_SCANNING_EXCLUDE_REPOS` are independent, so you can turn the flag off and still name single forks in the list. A skipped repository counts neither as read nor as failed, so it never marks a scan degraded and never hides a real code-scanning outage.

`PR_EXCLUDE_QUERY` and `ISSUE_EXCLUDE_QUERY` are added as written to GitHub's search, so any [search qualifier](https://docs.github.com/en/search-github/searching-on-github/searching-issues-and-pull-requests) works there. The defaults leave out pull requests opened by Renovate, and issues opened by Renovate or labeled `renovate` or `auto-generated`. A value you set replaces the default, so repeat the default terms you want to keep.

## How values are read

Every setting is read once when the container starts. A value github-scout cannot read falls back to its default, so a mistyped `SCAN_INTERVAL` keeps scanning every 15 minutes. A value out of range is moved to the nearest limit. `SCAN_INTERVAL` stays between 1 minute and 365 days, and `LOOKBACK_HOURS` between 1 and 720 hours.

`SCAN_INTERVAL` is a [Go duration](https://pkg.go.dev/time#ParseDuration), such as `90s`, `15m` or `2h`. Each scan starts up to 10% earlier or later than the interval. No value turns scanning off. `off`, `disabled` and `0` fall back to the default too, with an `invalid SCAN_INTERVAL, using default` warning.

`LOOKBACK_HOURS` also bounds how far back the dashboard can show failed runs, so keep the Grafana time range within it.

If `GITHUB_OWNER` or `GITHUB_TOKEN` is empty, or the owner holds characters a GitHub login cannot have, github-scout logs `invalid configuration; need GITHUB_OWNER and GITHUB_TOKEN` and exits with code 1.

## One-shot scans

`github-scout trigger` runs one scan, writes its lines to its own output and exits. The exit code is 1 when the configuration is invalid or repository discovery failed, and 0 otherwise. It suits a development loop (`go run . trigger`), cron on a host without Docker, and CI.

In a container, leave scheduling to the built-in timer. A scan started with `docker exec github-scout /github-scout trigger` writes to the exec session and not to the container log, so the dashboard and the alert rules never see it. A one-shot scan never touches the health marker.

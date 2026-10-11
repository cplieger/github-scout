# forge-scout

[![Image Size](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/cplieger/github-scout/badges/size.json)](https://github.com/cplieger/github-scout/pkgs/container/github-scout) [![Platforms](https://img.shields.io/badge/platforms-amd64%20%7C%20arm64-blue)](https://github.com/cplieger/github-scout/pkgs/container/github-scout) [![base: Distroless](https://img.shields.io/badge/base-Distroless_nonroot-4285F4?logo=google)](https://github.com/cplieger/github-scout/blob/main/Dockerfile) [![Mutation](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/cplieger/github-scout/badges/mutation.json)](https://github.com/cplieger/github-scout/issues?q=label%3Agremlins-tracker) [![SBOM](https://img.shields.io/badge/SBOM-SPDX-1D4ED8)](https://github.com/cplieger/github-scout/releases)

<!-- hub-overview BEGIN -->
forge-scout puts the open pull requests, open issues and failing CI workflows across your repositories on GitHub, GitLab, Gitea and Forgejo on one Grafana dashboard, with a link on every row, and adds code-scanning alerts on GitHub. It only reads from your forges.

![The forge-scout Grafana dashboard's Overview tab: count tiles for failing workflows, pull requests with failing checks, open pull requests and issues, code-scanning alerts and scan integrity, above the table of workflows failing on their default branch](docs/images/header.png)

## What it does

forge-scout shows you what is waiting for you across all your repositories and forges, in one place:

- Lists open pull requests and issues, idlest first, with who opened them, how old they are and whether a pull request's checks fail.
- Lists the workflows whose latest run on the default branch failed, how many runs in a row, since when and when they last ran.
- Logs every finished CI run, the slowest workflows and the workflows GitHub disabled.
- Lists open GitHub code-scanning alerts, colored by severity.
- Adds a new repository or workflow on the next scan, with nothing to configure.
- Marks the counts a scan could not read whole, and the signals a forge does not have, so the dashboard never shows an unchecked count as 0.

## Who it is for

forge-scout is built for people with repositories on one or more forges, private ones included: github.com or GitHub Enterprise Server, gitlab.com or a self-managed GitLab, and Gitea or Forgejo instances. It checks every repository of the owners you name every 15 minutes.

forge-scout reports through its log, so its dashboard and alerts come from your Grafana, Loki and Alertmanager, which the [monitoring guide](https://github.com/cplieger/docs/blob/main/docs/monitoring.md#the-smallest-stack-sends-notifications-only) sets up. You also need a read-only access token for each forge.

Other tools suit other needs:

- Consider the [Grafana GitHub data source](https://grafana.com/grafana/plugins/grafana-github-datasource/) if you want Grafana panels that query the GitHub API directly for your repositories and projects.
- Consider [gh-dash](https://github.com/dlvhdr/gh-dash) if you want to work through GitHub pull requests and issues from a terminal, with diff, comment and checkout built in.

forge-scout is free software under the GPL-3.0-or-later license.
<!-- hub-overview END -->

## Quick start

The image is on GitHub Container Registry and Docker Hub, for `amd64` and `arm64`. This is the [`compose.yaml`](compose.yaml) in this repository.

```yaml
services:
  forge-scout:
    image: ghcr.io/cplieger/github-scout:latest
    container_name: forge-scout
    restart: unless-stopped
    # Must own the ./data host folder. If .env sets PUID and PGID, those numbers are used.
    user: "${PUID:-1000}:${PGID:-1000}"

    environment:
      # Every token the config file names as ${...}. Put each in a .env file beside this file.
      # The github connection in config.example.yaml reads this token. Delete this line when you remove that connection.
      GITHUB_TOKEN: "${GITHUB_TOKEN:-}"
      # The forgejo connection in config.example.yaml reads this token. Delete this line when you remove that connection.
      FORGEJO_TOKEN: "${FORGEJO_TOKEN:-}"

    volumes:
      # Run "mkdir -p config && cp config.example.yaml config/config.yaml", then edit
      # config/config.yaml before the first start.
      - "./config:/config:ro"
      # The run state. Run "mkdir -p data && sudo chown 1000:1000 data" before the first start.
      # If .env sets PUID and PGID, use those numbers.
      - "./data:/data"
```

1. Create a read-only token on each forge, with the [permissions](docs/configuration.md#token-permissions) for that forge. On GitHub, use a classic token with the `repo` scope, which also reads pull request checks. A fine-grained token with read-only Actions, Contents, Commit statuses, Pull requests, Issues and Code scanning alerts works too, but GitHub gives it no permission for check runs, so the checks of pull requests on private repositories are not available with it.
2. In the folder that holds `compose.yaml`, create a file named `.env` with one line per token, such as `GITHUB_TOKEN=github_pat_your_token`. If your user on the host is not uid 1000, also add `PUID=` and `PGID=` lines with your numbers from `id -u` and `id -g`.
3. Save [`config.example.yaml`](config.example.yaml) beside `compose.yaml`, then run `mkdir -p config && cp config.example.yaml config/config.yaml`. In `config/config.yaml`, set each connection's `url` and `owners`, and delete the connections you do not use.
4. Add each token variable the config names to the `environment:` block of `compose.yaml`.
5. Run `mkdir -p data && sudo chown 1000:1000 data`, with your `PUID` and `PGID` in place of 1000 if you set them, then `docker compose up -d`.
6. Have your log collector [send the container's logs to Loki](https://github.com/cplieger/docs/blob/main/docs/monitoring.md#the-smallest-stack-sends-notifications-only) with the label `container="forge-scout"`, which every dashboard panel selects on.
7. In Grafana 13.2 or newer, [import](https://github.com/cplieger/docs/blob/main/docs/monitoring.md#importing-an-apps-dashboard) [`grafana-dashboard.json`](grafana-dashboard.json). It reads from your default Loki data source.

Run `docker logs forge-scout`. You should see one line with `"msg":"scan complete"` and `"degraded":false` for each connection. If a forge rejected the token or could not be reached, its connection logs `"msg":"repo discovery failed"`, then `"msg":"scan degraded"`, and still ends with `"msg":"scan complete"`, but with `"degraded":true`. The `connection` field names which one. A connection that stopped early logs `"msg":"scan stopped"` and no `scan complete`.

## Configuration reference

The settings are one YAML file, read once at start, so recreate the container after a change. Its path is `/config/config.yaml`, or the `CONFIG_PATH` variable. A value written as `${NAME}` is read from the environment when the name ends in `_TOKEN` or starts with `FORGE_SCOUT_`. Each `token` must be one such reference and nothing else, so no token is ever written into the file. An unknown key, or a second document in the file, stops the start with an error naming it.

The container reads these environment variables:

| Variable | Description | Default |
| --- | --- | --- |
| `CONFIG_PATH` | Path of the config file inside the container | `/config/config.yaml` |
| Each `${NAME}` the file names | The value of that reference, such as `GITHUB_TOKEN`. Set each one in the `environment:` block | none |
| `PUID`, `PGID` | The user and group the container runs as, who must own `./data`. `compose.yaml` reads them, not forge-scout | `1000` |

The file takes these keys:

| Key | Default | Description |
| --- | --- | --- |
| `scan_interval` | `15m` | Time between scans, such as `15m` or `1h`, from 1 minute to 171 hours |
| `lookback` | `72h` | How far back every scan reads CI runs, from `1h` to `720h`, at least 4.2 times `scan_interval`: two `scan_interval`, each up to 10% late, plus one scan at its limit of twice `scan_interval`. A re-run is seen while its run is younger than `lookback` |
| `log_level` | `info` | `debug` or `info`. `warn` and `error` stop the start, because every line the dashboard reads is logged at `info` |
| `connections` | required | One entry per forge instance, keys below |

Each connection takes these keys:

| Key | Default | Description |
| --- | --- | --- |
| `name` | required | The label every log line carries as `connection`. Lowercase letters, digits, `-` and `_`, up to 32 characters |
| `url` | required | The address you open the forge at, such as `https://github.com` |
| `api_url` | derived from `url` | The API address, for an instance that serves it somewhere else |
| `token` | required | A read-only token, written as exactly one `${NAME}` reference to an environment variable |
| `owners` | required | The users, organizations or GitLab groups whose repositories are scanned |
| `exclude_repos` | none | Full `owner/name` paths left out of every signal |
| `exclude_authors` | none | Logins whose open pull requests and issues are left out |
| `exclude_labels` | none | Labels whose open pull requests and issues are left out |
| `bot_authors` | none | Logins shown as bots, beside any login ending in `[bot]` |
| `security` | `enabled: true`, `skip_forks: true`, `include_private: false` | `enabled`, `skip_forks`, `include_private` and `skip_repos` for code-scanning alerts |
| `private_addresses` | `false` | Allow an instance on a private or loopback address |
| `allow_plaintext` | `false` | Allow an `http://` address |

forge-scout detects which product each connection runs, so nothing in the file names it. [Configuration](docs/configuration.md) explains the tokens, the owners, the exclusions and the one-shot `trigger` command.

## What each forge supports

Pull requests, issues and CI runs are read on every forge. Code-scanning alerts and disabled workflows exist on GitHub alone, so on the other forges those counts are left out of the log and the dashboard shows them as not available, never as 0.

- On Gitea and Forgejo, the run listing has no time filter. forgeapi ends it at the first page whose runs were all created more than a minute before the listing's start, so a long run history costs one or two requests a scan. Gitea releases before 28 send no run creation time, so their runs are not read and the log says so once per scan.
- Run durations need a start time from the forge. GitHub, Gitea and Forgejo send one. GitLab's pipeline list does not, so GitLab workflows never appear among the slowest workflows.
- A GitLab owner that is a user and not a group has no owner-wide pull request or issue list. forge-scout then reads each of that user's projects, which costs two requests per project per scan.
- A pull request's checks are read on GitHub and GitLab, one request per open pull request. GitHub refuses a fine-grained token the checks of a pull request on a private repository, and the dashboard shows them as Not available with this token. Gitea and Forgejo name no head commit in the cross-repository list, so their pull requests show no checks.

## Security

forge-scout opens no port and runs no web server. It sends only read requests, and only to the forges you configure, so give it read-only tokens. Each token travels only in the request header to its own forge and never appears in the log, which records only whether a token is set. Keep `.env` out of git.

forge-scout reads at most 8 MiB of each response body, on every forge and for its own GitHub code-scanning and workflow reads alike. A larger response fails that read, so a hostile or broken server cannot fill its memory. One repository's code-scanning alerts are held to 8 MiB too, across all their pages: past that, the repository's code-scanning read fails.

forge-scout holds back 250 requests of the budget each forge's last response reported. A scan that would go below that stops for that forge, logs `scan stopped` and reads again once the budget renews, so it does not use up a budget your other tools share. GitHub keeps separate budgets for its REST and GraphQL APIs and reports only the one a request drew on. There forge-scout holds the 250 against the REST budget that all of the connection's reads share, and forgeapi holds the GraphQL budget itself. A GitHub answer of 403 with no rate-limit header stops every request of that connection for the rest of the scan, because GitHub sends it for a secondary rate limit too.

The image is distroless, with no shell, and runs as a non-root user. It writes only to `/data`, where it keeps the run state, and to `/tmp`, where it keeps a health marker. [Security](docs/hardening.md) has the hardened compose settings and what the image contains.

## Troubleshooting

The healthcheck runs `/forge-scout health`, which checks that the scan loop refreshed its marker file within the last three scan intervals plus one scan at its limit of twice the interval, 75 minutes at the default. Unhealthy means the loop stopped, so restart the container. The container stays healthy through a failed scan, a rejected token or a spent budget. Those appear in the log, and the bundled alert rules fire on them.

- If the container exits at start with `invalid configuration`, the error names the key or the connection to fix. A missing file names `/config/config.yaml`.
- If the log shows `"cause":"token_invalid"`, that forge rejected the token. Check that it has not expired.
- If the log shows `"cause":"owner_unresolved"`, that forge does not know one of the `owners`. The `reason` field names it.
- If the log shows `owner lists no repository`, the `owners` field names owners that exist but hold no repository this token can see. A misspelt owner that happens to name another account shows up here, as does a token that cannot see the owner's private repositories, and a GitLab user whose profile is private.
- If the log shows `uses the same account as`, two connections to one forge use tokens of the same account. List both connections' owners in the first one and remove the second.
- If every scan logs `code scanning unreadable` for the same repository and `"cause":"github_refused"`, GitHub refuses that repository's code scanning, as it does for a private repository without GitHub Advanced Security once `security.include_private` is on. Add its path to `security.skip_repos`.
- If the log shows `"cause":"github_refused"` after `runs listing failed` or `pull request checks unreadable`, the token lacks a permission on that repository. Its `hint` field names the one to grant.
- If the log shows `runs coverage gap`, forge-scout was stopped for longer than `lookback`, or its run lists of that repository stayed cut short for that long, and the CI runs created in the `hours` before the window were not read. Raise `lookback` if such gaps are expected. When the line carries `pending_since`, a list cut short no longer reached a run still in progress at the scan before, so that run's result may never be logged.
- If the log shows `run state unreadable; starting cold`, the file in `/data` could not be used and was moved to the `set_aside` path, so the first scan reports the whole lookback window again. The `error` field says why. Check that `./data` belongs to the `PUID` and `PGID` the container runs as, 1000 unless `.env` sets them.
- If the container exits at start with `run state directory unusable`, forge-scout could not create its lock file in `/data`. The `error` field says why. Check that `./data` exists and belongs to the `PUID` and `PGID` the container runs as.
- If the log shows `run state save failed`, `/data` became unwritable or full after forge-scout started. Each scan logs `"cause":"state_unwritable"`. CI runs are still logged, and a restart before a save succeeds logs them again. The `error` field says why.
- If the log shows `run state not durable`, the file system under `/data` cannot sync a folder, as some network shares cannot. Each scan logs `"cause":"state_not_durable"`. CI runs are still reported, but a power loss can make the next start report some of them again. Put `/data` on a local disk.
- If the container exits at start with `state directory is in use by another forge-scout process`, another forge-scout uses the same `/data`. Give each container its own folder, and run `trigger` only when the daemon is stopped.

## Monitoring

forge-scout writes one JSON log line for each open pull request, issue, alert and observed run state, plus a `scan complete` summary for each connection after each scan. The bundled dashboard needs Grafana 13.2 or newer and reads those lines from Loki, and two Loki alert rules fire when scans go blind or stop. [Monitoring and alerts](docs/monitoring.md) lists the log fields, the dashboard and the rules.

## Documentation

- [Configuration](docs/configuration.md) covers the token permissions, the owners, the exclusions and one-shot scans.
- [Monitoring and alerts](docs/monitoring.md) covers the log lines, the Grafana dashboard and the alert rules.
- [Security](docs/hardening.md) covers the hardened compose settings and what the image contains.
- [How forge-scout works](docs/how-it-works.md) covers scanning, the run state and API use.

## Credits

forge-scout reads every forge through [forgeapi](https://github.com/cplieger/forgeapi), and GitHub code scanning and workflows through the [GitHub REST API](https://docs.github.com/en/rest). The way that GitHub client authenticates, sets the API version and pages through results follows two Prometheus exporters for GitHub, [githubexporter/github-exporter](https://github.com/githubexporter/github-exporter) and [xrstf/github_exporter](https://github.com/xrstf/github_exporter). [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md) names each pattern.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md).

## Disclaimer

This project is built with care and follows security best practices, but it is intended for personal / self-hosted use. No guarantees of fitness for production environments. Use at your own risk.

This project was built with AI-assisted tooling using [Claude](https://claude.com), [GPT](https://openai.com), and [Kiro](https://kiro.dev). The human maintainer defines architecture, supervises implementation, and makes all final decisions.

## License

GPL-3.0-or-later. See [LICENSE](LICENSE). The image carries the license text of every bundled component under `/usr/share/licenses/`.

Third-party attributions are in [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).

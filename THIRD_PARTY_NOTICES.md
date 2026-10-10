# Third-party notices

## Design followed, no code included

The GitHub REST API client in `internal/githubrest` follows patterns common to two MIT-licensed community exporters. No source code was copied; the approach was studied and reimplemented. Each pattern is named at the line of ours that follows it:

- The request headers in `internal/githubrest/githubrest.go` (`attempt`: the `Authorization: Bearer` set and the `X-GitHub-Api-Version` pin) follow both exporters.
- The page-count pagination in `internal/githubrest/githubrest.go` (`CodeScanningAlerts` and `Workflows` each request `page` with `per_page` and stop on a short page) follows both exporters.

The upstreams:

- [githubexporter/github-exporter](https://github.com/githubexporter/github-exporter), copyright (c) 2016 Infinity Works Ltd, MIT.
- [xrstf/github_exporter](https://github.com/xrstf/github_exporter), copyright (c) 2020 Christoph Mewes, MIT.

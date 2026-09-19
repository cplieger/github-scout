# Third-party notices

## Design followed, no code included

The GitHub REST API client in `internal/github` follows patterns common to two MIT-licensed community exporters. No source code was copied; the approach was studied and reimplemented. Each pattern is named at the line of ours that follows it:

- The request headers in `internal/github/client.go` (`setHeaders`: the `Authorization: Bearer` set and the `X-GitHub-Api-Version` pin) follow both exporters.
- The page-count pagination in `internal/github/client.go` (`ListRepos`, `ListRuns`, the `/search/issues` paths and `ListCodeScanningAlerts` each request `page` with `per_page` and stop on a short page) follows both exporters.

The upstreams:

- [githubexporter/github-exporter](https://github.com/githubexporter/github-exporter), copyright (c) 2016 Infinity Works Ltd, MIT.
- [xrstf/github_exporter](https://github.com/xrstf/github_exporter), copyright (c) 2020 Christoph Mewes, MIT.

The MIT License permits this use. Their full license text is reproduced below for attribution.

```text
MIT License

Permission is hereby granted, free of charge, to any person obtaining a
copy of this software and associated documentation files (the
"Software"), to deal in the Software without restriction, including
without limitation the rights to use, copy, modify, merge, publish,
distribute, sublicense, and/or sell copies of the Software, and to permit
persons to whom the Software is furnished to do so, subject to the
following conditions:

The above copyright notice and this permission notice shall be included
in all copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL
THE AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING
FROM, OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER
DEALINGS IN THE SOFTWARE.
```

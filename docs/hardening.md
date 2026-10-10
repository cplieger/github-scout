# Security

This page covers what forge-scout exposes, how it handles tokens, the hardened compose settings and what the image contains. It is for operators who want to lock a deployment down further than the quick start.

## What it exposes

- forge-scout runs no HTTP server and listens on no port, so nothing on the network can reach it. Its output is standard output, and its health is a file marker.
- It sends only read requests, and only to the forges in its config file. Every connection is opened with writes refused, and it reads at most 8 MiB of each response body. A larger response fails that read.
- A connection refuses a private or loopback address unless it sets `private_addresses: true`, and an `http://` address unless it sets `allow_plaintext: true`.
- Owner and repository names are checked before they go into a request URL, which rejects path traversal and injection characters.
- Each token is sent only to its own forge, as a request header, and is never logged. The startup lines log `token_set` with `true` or `false` instead. A config error names the variable a token comes from, never its value.
- A config error or warning about any other value read from a `${NAME}` reference names the reference, such as `${FORGE_SCOUT_LOOKBACK}`, never the value it read.

Because forge-scout only reads, a read-only token is all it needs. [Configuration](configuration.md#token-permissions) lists the permissions per forge.

## Hardened deployment

[Pin the image by digest](https://github.com/cplieger/docs/blob/main/docs/images.md#pinning-a-digest) and add these lines to the quick start service. [Hardening a compose file](https://github.com/cplieger/docs/blob/main/docs/hardening.md) explains each setting.

```yaml
    read_only: true
    cap_drop:
      - ALL
    security_opt:
      - no-new-privileges:true
    tmpfs:
      - "/tmp:size=1m,mode=1777,noexec,nosuid,nodev"
```

With `read_only: true`, the health marker needs the tmpfs at `/tmp`, and the run state stays on the `./data` volume, which keeps it across restarts.

## What the image contains

The image is built on distroless static and runs as the `nonroot` user, uid 65532, with no shell and no package manager. The example compose runs it as `PUID` and `PGID` instead, 1000 unless `.env` sets them, so the run state on `./data` stays owned by your host user. The image holds one static Go binary, empty `/config` and `/data` folders owned by that user, and the license text of every bundled component under `/usr/share/licenses/`. Its Go runtime dependencies are libraries by the same author, plus the YAML parser `go.yaml.in/yaml/v3`. The image is built from these:

| Dependency | Source |
| --- | --- |
| golang | [Go](https://hub.docker.com/_/golang), the build stage only |
| Distroless static | [Distroless](https://github.com/GoogleContainerTools/distroless) |
| cplieger/forgeapi | [forgeapi](https://github.com/cplieger/forgeapi), the client for every forge |
| cplieger/urlform | [urlform](https://github.com/cplieger/urlform), the check forgeapi runs on each connection's `url` before it sends a request |
| cplieger/httpx | [httpx](https://github.com/cplieger/httpx), the retrying HTTP client under forgeapi and the GitHub code-scanning and workflow reads |
| cplieger/jsoncap | [jsoncap](https://github.com/cplieger/jsoncap), the size and item limits on decoded responses, forgeapi's and the GitHub code-scanning and workflow pages |
| cplieger/ssrf | [ssrf](https://github.com/cplieger/ssrf), the private-address check on every connection's requests |
| cplieger/envx | [envx](https://github.com/cplieger/envx), the `${NAME}` expansion in the config file |
| go.yaml.in/yaml/v3 | [yaml](https://github.com/yaml/go-yaml), the config file parser |
| cplieger/atomicfile | [atomicfile](https://github.com/cplieger/atomicfile), the crash-safe run state file |
| cplieger/health | [health](https://github.com/cplieger/health), the file-marker probe |
| cplieger/scheduler | [scheduler](https://github.com/cplieger/scheduler), the scan loop |
| cplieger/slogx | [slogx](https://github.com/cplieger/slogx), the log setup |
| cplieger/runesafe | [runesafe](https://github.com/cplieger/runesafe), the sanitizer for text read from the forges |

[Renovate](https://github.com/renovatebot/renovate) updates every dependency, and each is pinned by digest or version so a build can be reproduced.

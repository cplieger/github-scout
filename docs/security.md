# Security

This page covers what github-scout exposes, how it handles the token, the hardened compose settings and what the image contains. It is for operators who want to lock a deployment down further than the quick start.

## What it exposes

- github-scout runs no HTTP server and listens on no port, so nothing on the network can reach it. Its output is standard output, and its health is a file marker.
- It sends only GET requests, and only to `api.github.com`, through a client that retries with backoff and caps each response body at 8 MB.
- Owner and repository names are checked before they go into a request URL, which rejects path traversal and injection characters.
- The token is sent only to `api.github.com`, as a Bearer header, and is never logged. The startup line logs `token_set` with `true` or `false` instead.

Because github-scout only reads, a read-only token is all it needs. [Configuration](configuration.md#token-permissions) lists the permissions.

## Hardened deployment

To lock the container down further, pin the image by digest and add these settings to the quick start service:

```yaml
    read_only: true
    cap_drop:
      - ALL
    security_opt:
      - no-new-privileges:true
    tmpfs:
      - "/tmp:size=16m,mode=1777,noexec,nosuid,nodev"
```

With `read_only: true`, the health marker and the two state files need the tmpfs at `/tmp`, and `size=16m` holds all three. Without `read_only`, no tmpfs is needed. Docker empties a tmpfs on every container start, so with this profile each start logs the runs inside the lookback window again, as [How github-scout works](how-it-works.md#state) describes.

## What the image contains

The image is built on distroless static and runs as the `nonroot` user, with no shell and no package manager. It holds one static Go binary and the license text of every bundled component under `/usr/share/licenses/`. Its Go runtime dependencies are all libraries by the same author. The image is built from these:

| Dependency | Source |
| --- | --- |
| golang | [Go](https://hub.docker.com/_/golang), the build stage only |
| Distroless static | [Distroless](https://github.com/GoogleContainerTools/distroless) |
| cplieger/httpx | [httpx](https://github.com/cplieger/httpx), the retrying HTTP client |
| cplieger/health | [health](https://github.com/cplieger/health), the file-marker probe |
| cplieger/scheduler | [scheduler](https://github.com/cplieger/scheduler), the scan loop and the state files |
| cplieger/slogx | [slogx](https://github.com/cplieger/slogx), the log setup |
| cplieger/envx | [envx](https://github.com/cplieger/envx), the environment-variable readers |
| cplieger/runesafe | [runesafe](https://github.com/cplieger/runesafe), the sanitizer for text read from GitHub |

[Renovate](https://github.com/renovatebot/renovate) updates every dependency, and each is pinned by digest or version so a build can be reproduced.

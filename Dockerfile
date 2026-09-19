# check=error=true
FROM golang:1.27-alpine@sha256:7d5cbf6833f7331dafd25a2e8b9673477f559759ff8ed4ca8efabe6795ad08db AS builder
ENV GOTOOLCHAIN=auto

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
COPY internal/ internal/
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /github-scout .
COPY LICENSE NOTICE THIRD_PARTY_NOTICES.md ./
COPY scripts/collect-licenses.sh scripts/
RUN sh scripts/collect-licenses.sh --name github-scout .

FROM gcr.io/distroless/static-debian13:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3

COPY --chmod=755 --from=builder /github-scout /github-scout
COPY --from=builder /out/usr/share/licenses /usr/share/licenses
USER nonroot:nonroot
HEALTHCHECK --interval=30s --timeout=5s --retries=3 --start-period=15s \
    CMD ["/github-scout", "health"]
ENTRYPOINT ["/github-scout"]

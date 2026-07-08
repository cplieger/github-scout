#!/bin/sh
# Runtime image smoke test for github-scout. Invoked by the central CI docker job:
#   sh tests/image-smoke.sh <image-ref>
#
# github-scout is a stateless distroless watcher with no port and no /metrics
# endpoint; its only runtime contract is the file-marker HEALTHCHECK
# (`/github-scout health` stats /tmp/.healthy). In the default scheduled mode
# it reaches "healthy" only after a scan whose repo discovery succeeds, which
# needs a valid GITHUB_TOKEN and live api.github.com access -- unavailable in
# CI. So this test runs the image in resident-idle mode (SCAN_INTERVAL=off):
# main.go writes the health marker and idles WITHOUT scanning (no GitHub call,
# buildCollector only constructs the client), so the assembled image reaches
# healthy on dummy credentials. That proves the real assembly facts a unit
# test cannot: the static binary runs in the distroless nonroot base, /tmp is
# writable for the marker, embedded tzdata loads, and the `health` subcommand
# works end to end against the shipped HEALTHCHECK.
#
# GITHUB_OWNER / GITHUB_TOKEN must be non-empty (internal/config Valid()) and
# the owner URL-safe (internal/urlsafe); the values are never used because no
# scan runs in resident-idle mode.
set -eu

IMG="${1:?usage: image-smoke.sh <image-ref>}"
NAME="smoke-github-scout-$$"
TIMEOUT=60 # covers the 15s HEALTHCHECK start-period + the first 30s probe interval + margin

# shellcheck disable=SC2329  # invoked indirectly via trap
cleanup() {
  code=$?
  # Dump container logs only on failure (a passing run stays quiet).
  if [ "$code" -ne 0 ]; then
    printf '%s\n' "--- container logs (tail) ---" >&2
    docker logs "$NAME" 2>&1 | tail -40 >&2 || true
  fi
  docker rm -f "$NAME" >/dev/null 2>&1 || true
}
trap cleanup EXIT

# Resident-idle (SCAN_INTERVAL=off) writes the health marker without scanning,
# so the image reaches healthy without a real token or network access. Both
# env values are unused (no scan runs); the owner is URL-safe per urlsafe.
docker run -d --name "$NAME" \
  -e GITHUB_OWNER=smoke-test \
  -e GITHUB_TOKEN=smoke-dummy-token \
  -e SCAN_INTERVAL=off \
  "$IMG" >/dev/null

i=0
status=starting
while [ "$i" -lt "$TIMEOUT" ]; do
  # Fail fast on an early exit: poll .State.Running before the health status so
  # a crash-boot is caught by its exit code (more debuggable than "unhealthy")
  # and the verdict never depends on what health a stopped container reports.
  if [ "$(docker inspect --format '{{ .State.Running }}' "$NAME" 2>/dev/null || echo missing)" != "true" ]; then
    ec=$(docker inspect --format '{{ .State.ExitCode }}' "$NAME" 2>/dev/null || echo '?')
    printf 'FAIL: github-scout container exited early (exit code %s)\n' "$ec" >&2
    exit 1
  fi
  status=$(docker inspect --format '{{ if .State.Health }}{{ .State.Health.Status }}{{ else }}no-healthcheck{{ end }}' "$NAME" 2>/dev/null || echo gone)
  case "$status" in
    healthy)
      printf 'github-scout image smoke: ok (healthy after %ss)\n' "$i"
      exit 0
      ;;
    unhealthy)
      printf 'FAIL: github-scout reported unhealthy\n' >&2
      exit 1
      ;;
    no-healthcheck)
      printf 'FAIL: image has no HEALTHCHECK to assert against\n' >&2
      exit 1
      ;;
    gone)
      printf 'FAIL: github-scout container is gone\n' >&2
      exit 1
      ;;
  esac
  i=$((i + 1))
  sleep 1
done
printf 'FAIL: github-scout did not become healthy within %ss (last status: %s)\n' "$TIMEOUT" "$status" >&2
exit 1

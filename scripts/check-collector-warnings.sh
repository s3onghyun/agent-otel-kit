#!/usr/bin/env bash
# Start the pinned collector image with the shipped config for a few seconds
# and fail if its log reports deprecated component aliases, deprecated or
# invalid config keys, or unknown types. `otelcol validate` is silent about
# deprecated aliases and keys; they only show up as warnings at start-up.
# Needs docker.
#
#   scripts/check-collector-warnings.sh
#
# Checked: collector/config.yaml, and config.yaml + debug.yaml (make up-debug).
# The collector runs without the rest of the stack and without a network, so
# exporters cannot connect; only config-level messages are looked at.
#
# Self-test: collector/testdata/deprecated-alias.yaml uses a deprecated alias
# and must be flagged, which proves the check still matches what the image
# logs. If a future image drops that alias, pick another deprecated one.
#
# CHECK_SECONDS sets how long each run lasts (default 5).
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
image="$(sed -n 's/^OTELCOL_IMAGE := //p' "$root/Makefile")"
seconds="${CHECK_SECONDS:-5}"
pattern='deprecated|invalid keys|unknown (type|key|field|option)|^Error:'
name="agent-otel-kit-collector-warnings-$$"
cleanup() { docker rm -f "$name" >/dev/null 2>&1 || true; }
trap cleanup EXIT

if [ -z "$image" ]; then
  echo "OTELCOL_IMAGE not found in Makefile" >&2
  exit 2
fi

# run_case LABEL EXPECT CONFIG...
#   EXPECT=clean   fail on any matching log line or an early exit
#   EXPECT=flagged fail unless a "deprecated" line is logged
run_case() {
  local label="$1" expect="$2"
  shift 2
  local args=() c
  for c in "$@"; do args+=("--config=/etc/otelcol/$c"); done

  cleanup
  # Same mount, path and environment as compose.yaml.
  docker run -d --name "$name" --network none \
    -e AGENT_OTEL_KIT_CAPTURE_CONTENT=false \
    -e AGENT_OTEL_KIT_KEEP_EMAIL=false \
    -v "$root/collector:/etc/otelcol:ro" \
    "$image" "${args[@]}" >/dev/null

  local status=""
  status="$(timeout "$seconds" docker wait "$name" 2>/dev/null)" || true
  local log
  log="$(docker logs "$name" 2>&1)"
  cleanup

  local hits
  hits="$(grep -iE "$pattern" <<<"$log" || true)"

  case "$expect" in
    clean)
      if [ -n "$hits" ] || [ -n "$status" ]; then
        echo "FAIL $label"
        if [ -n "$status" ]; then echo "  collector exited after start (code $status)"; fi
        if [ -n "$hits" ]; then
          sed 's/^/  /' <<<"$hits"
        else
          tail -n 20 <<<"$log" | sed 's/^/  /'
        fi
        return 1
      fi
      echo "ok   $label: no deprecated, invalid or unknown config warnings in ${seconds}s"
      ;;
    flagged)
      if ! grep -qi 'deprecated' <<<"$hits"; then
        echo "FAIL self-test $label: expected a deprecation warning, got none"
        tail -n 20 <<<"$log" | sed 's/^/  /'
        return 1
      fi
      echo "ok   self-test $label: deprecation warning detected"
      ;;
  esac
}

fails=0
run_case "config.yaml" clean config.yaml || fails=1
run_case "config.yaml + debug.yaml" clean config.yaml debug.yaml || fails=1
run_case "testdata/deprecated-alias.yaml" flagged testdata/deprecated-alias.yaml || fails=1

if [ "$fails" -ne 0 ]; then
  echo "collector warning check FAILED (image $image)"
  exit 1
fi
echo "collector warning check ok (image $image)"

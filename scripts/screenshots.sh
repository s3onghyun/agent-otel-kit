#!/usr/bin/env bash
# Capture the dashboards of a running stack with headless Chrome/Chromium.
# The images show whatever data the stack holds; run make demo first.
#
#   scripts/screenshots.sh [output-dir]       (default docs/screenshots)
#
# Environment: CHROME (browser binary), RANGE (default now-1h), TRACE_ID
# (trace shown in the trace panel; default: the newest trace in Tempo).
set -euo pipefail

out="${1:-docs/screenshots}"
range="${RANGE:-now-1h}"
grafana="${GRAFANA_URL:-http://127.0.0.1:3000}"
perses="${PERSES_URL:-http://127.0.0.1:8080}"
tempo="${TEMPO_URL:-http://127.0.0.1:3200}"

chrome="${CHROME:-}"
if [[ -z "$chrome" ]]; then
  for c in google-chrome google-chrome-stable chromium chromium-browser; do
    if command -v "$c" >/dev/null; then chrome="$c"; break; fi
  done
fi
[[ -n "$chrome" ]] || { echo "screenshots: no Chrome/Chromium found; set CHROME" >&2; exit 1; }

trace="${TRACE_ID:-}"
if [[ -z "$trace" ]]; then
  trace="$(curl -fsS -G "$tempo/api/search" --data-urlencode 'q={resource.agent.name=~".+" && name="claude_code.interaction"}' --data-urlencode limit=1 \
    | sed -n 's/.*"traceID":"\([0-9a-f]*\)".*/\1/p' | head -n1 || true)"
fi

mkdir -p "$out"
profile="$(mktemp -d "${TMPDIR:-/tmp}/agent-otel-kit-chrome.XXXXXX")"
trap 'rm -rf -- "$profile"' EXIT

shot() { # name url width height
  echo "screenshots: $1" >&2
  "$chrome" --headless=new --no-first-run --no-default-browser-check --disable-gpu \
    --hide-scrollbars --user-data-dir="$profile" --force-device-scale-factor=1 \
    --window-size="$3,$4" --virtual-time-budget=20000 \
    --screenshot="$out/$1.png" "$2" >/dev/null 2>&1
}

g="from=$range&to=now&kiosk&refresh="
shot grafana-cost-tokens "$grafana/d/agent-cost-tokens/agent-cost-and-tokens?$g" 1600 1180
shot grafana-tool-calls "$grafana/d/agent-tool-calls/agent-tool-calls?$g" 1600 1180
shot grafana-sessions-traces "$grafana/d/agent-sessions/agent-sessions-and-traces?$g&var-trace=$trace" 1600 1640

if curl -fsS -o /dev/null "$perses/api/v1/health" 2>/dev/null; then
  p="start=${range#now-}&refresh=0s"
  shot perses-cost-tokens "$perses/projects/agent-otel-kit/dashboards/agent-cost-tokens?$p" 1600 1180
  shot perses-tool-calls "$perses/projects/agent-otel-kit/dashboards/agent-tool-calls?$p" 1600 1240
  shot perses-sessions-traces "$perses/projects/agent-otel-kit/dashboards/agent-sessions?$p&var-trace=$trace" 1600 1240
else
  echo "screenshots: Perses not running, skipped (docker compose --profile perses up -d)" >&2
fi
echo "screenshots: written to $out" >&2

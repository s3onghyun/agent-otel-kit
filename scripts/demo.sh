#!/usr/bin/env bash
# Generate real demo telemetry by running short, non-interactive Claude Code
# sessions against the local stack. Uses only the environment of this command;
# ~/.claude/settings.json is not read or changed by this script.
#
#   scripts/demo.sh            two sessions (haiku, sonnet)
#   MODELS="haiku" scripts/demo.sh
#
# Each session costs a few cents of your own Claude usage.
set -euo pipefail

endpoint="${AGENT_OTEL_KIT_ENDPOINT:-http://127.0.0.1:4318}"
models="${MODELS:-haiku sonnet}"

command -v claude >/dev/null || { echo "demo: claude CLI not found" >&2; exit 1; }

work="$(mktemp -d "${TMPDIR:-/tmp}/agent-otel-kit-demo.XXXXXX")"
trap 'rm -rf -- "$work"' EXIT
cat >"$work/calc.py" <<'PY'
def add(a, b):
    return a + b


def div(a, b):
    return a / b
PY
printf '# demo\nA scratch project used to generate telemetry.\n' >"$work/README.md"

prompt='List the files here with ls, read calc.py, then try to read missing.txt (it does not exist). Reply with one sentence about what calc.py does.'

for model in $models; do
  echo "demo: running a claude -p session with model=$model" >&2
  ( cd "$work" && env \
      CLAUDE_CODE_ENABLE_TELEMETRY=1 \
      CLAUDE_CODE_ENHANCED_TELEMETRY_BETA=1 \
      OTEL_METRICS_EXPORTER=otlp \
      OTEL_LOGS_EXPORTER=otlp \
      OTEL_TRACES_EXPORTER=otlp \
      OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf \
      OTEL_EXPORTER_OTLP_ENDPOINT="$endpoint" \
      OTEL_METRIC_EXPORT_INTERVAL=5000 \
      claude -p --model "$model" --allowedTools "Read" "Glob" "Bash(ls:*)" -- "$prompt" </dev/null )
done
echo "demo: done; metrics appear in Prometheus within ~15 s" >&2

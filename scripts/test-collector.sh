#!/usr/bin/env bash
# Run the collector config against recorded agent payloads and check what
# leaves the processors: agent.name normalization, tool fields, the derived
# tool histogram, content redaction, and the Claude Code model suffix and cost
# basis. Needs docker and python3.
#
#   scripts/test-collector.sh
#
# The payloads in collector/testdata/ have the shape real Codex 0.169.0,
# Gemini CLI 0.68.0 and Qwen Code 0.27.4 sessions sent (IDs and values
# replaced). The Qwen Code logs were recorded with logPrompts on, so they
# carry prompt and response fields for the redaction checks.
# claude-thirdparty-*.json were recorded from Claude Code 2.1.325 runs against
# a mock Anthropic-compatible endpoint (IDs replaced), one session each:
#   ...01 ANTHROPIC_MODEL=deepseek-chat (unknown model ID)
#   ...02 no model set, the endpoint answers as glm-4.6 (server-side mapping)
#   ...03 ANTHROPIC_MODEL=deepseek-flash[1m] (provider-guide suffix)
#   ...04 as 03, with managed modelPricing and OTEL_RESOURCE_ATTRIBUTES=cost.basis=managed
#   ...00 a copy of 02 with anthropic.custom_base_url=false, the value Claude
#         Code sends without ANTHROPIC_BASE_URL
# claude-thirdparty-mixed-metrics.json is derived, not recorded: session 03's
# metrics as session ...05, plus a second point per metric for model
# deepseek-flash (no suffix) in the same request with the same timestamps, as
# when a provider guide sets OPUS/SONNET to deepseek-flash[1m] and HAIKU to
# deepseek-flash. After the suffix strip both must add up to one point.
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
image="$(sed -n 's/^OTELCOL_IMAGE := //p' "$root/Makefile")"
out="$(mktemp -d "${TMPDIR:-/tmp}/agent-otel-kit-collector-test.XXXXXX")"
chmod 777 "$out"
name="agent-otel-kit-collector-test-$$"
cleanup() {
  docker rm -f "$name" >/dev/null 2>&1 || true
  rm -rf -- "$out"
}
trap cleanup EXIT

docker run -d --name "$name" -p 127.0.0.1::4318 \
  -v "$root/collector:/c:ro" -v "$out:/out" \
  "$image" --config=/c/config.yaml --config=/c/test.yaml >/dev/null
port="$(docker port "$name" 4318/tcp | head -n1 | sed 's/.*://')"

for _ in $(seq 1 30); do
  if curl -fsS -o /dev/null -X POST -H 'Content-Type: application/json' -d '{}' "http://127.0.0.1:$port/v1/logs" 2>/dev/null; then break; fi
  sleep 1
done

for f in "$root"/collector/testdata/*.json; do
  case "$f" in
    *-logs.json) signal=logs ;;
    *-metrics.json) signal=metrics ;;
    *-traces.json) signal=traces ;;
    *) continue ;;
  esac
  curl -fsS -o /dev/null -X POST -H 'Content-Type: application/json' --data-binary "@$f" "http://127.0.0.1:$port/v1/$signal"
done

# batch timeout is 2s, signal_to_metrics output goes through a second batch.
sleep 6
docker stop -t 5 "$name" >/dev/null

python3 - "$out" <<'PY'
import json, os, sys

out = sys.argv[1]

def lines(name):
    p = os.path.join(out, name)
    if not os.path.exists(p):
        return []
    with open(p) as f:
        return [json.loads(l) for l in f if l.strip()]

def attrs(kv):
    res = {}
    for a in kv or []:
        v = a.get("value", {})
        res[a["key"]] = next(iter(v.values()), None) if v else None
    return res

fails = []
def check(cond, msg):
    if not cond:
        fails.append(msg)

logs = []
for batch in lines("logs.json"):
    for rl in batch.get("resourceLogs", []):
        res = attrs(rl.get("resource", {}).get("attributes"))
        for sl in rl.get("scopeLogs", []):
            for lr in sl.get("logRecords", []):
                logs.append((res, attrs(lr.get("attributes"))))

points = []
for batch in lines("metrics.json"):
    for rm in batch.get("resourceMetrics", []):
        res = attrs(rm.get("resource", {}).get("attributes"))
        for sm in rm.get("scopeMetrics", []):
            for m in sm.get("metrics", []):
                for kind in ("sum", "gauge", "histogram"):
                    for dp in m.get(kind, {}).get("dataPoints", []):
                        points.append((m["name"], res, attrs(dp.get("attributes")), dp))

spans = []
for batch in lines("traces.json"):
    for rs in batch.get("resourceSpans", []):
        res = attrs(rs.get("resource", {}).get("attributes"))
        for ss in rs.get("scopeSpans", []):
            for sp in ss.get("spans", []):
                spans.append((sp["name"], res, attrs(sp.get("attributes"))))

def log_of(event):
    return [(r, a) for r, a in logs if a.get("event.name") == event]

# Codex
recs = log_of("codex.tool_result")
check(len(recs) == 1, f"codex.tool_result records: {len(recs)}")
for res, a in recs:
    check(res.get("agent.name") == "codex", f"codex resource agent.name = {res.get('agent.name')}")
    check(a.get("tool.name") == "exec_command", f"codex tool.name = {a.get('tool.name')}")
    check(a.get("tool.success") == "true", f"codex tool.success = {a.get('tool.success')}")
    check("agent_name" not in a, "codex agent_name attribute not removed")
    check(a.get("subagent.name") == "/root", f"codex subagent.name = {a.get('subagent.name')}")
    check(a.get("session.id") == "00000000-0000-7000-8000-000000000001", f"codex session.id = {a.get('session.id')}")
    check("output" not in a, "codex tool output not redacted")
for res, a in log_of("codex.tool_decision"):
    check("tool.name" not in a, "tool.name set on a decision event (would double count tool calls)")

tok = [(a.get("type"), dp.get("sum")) for n, r, a, dp in points if n == "codex.turn.token_usage"]
check(sorted(tok) == [("input", 7019), ("output", 225)], f"codex token points (type, sum) = {sorted(tok)}")

# Gemini CLI
recs = log_of("gemini_cli.tool_call")
check(len(recs) == 1, f"gemini_cli.tool_call records: {len(recs)}")
for res, a in recs:
    check(res.get("agent.name") == "gemini-cli", f"gemini resource agent.name = {res.get('agent.name')}")
    check(a.get("tool.name") == "read_file", f"gemini tool.name = {a.get('tool.name')}")
    check(a.get("tool.success") is False, f"gemini tool.success = {a.get('tool.success')}")
    check("function_args" not in a, "gemini function_args not redacted")
    check("error.message" not in a, "gemini error.message not redacted")
    check(a.get("error.type") == "file_not_found", "gemini error.type removed")
    check("process.command_args" not in res, "gemini process.command_args not redacted")
    check(res.get("process.command") == "/home/user/.local/bin/gemini", "gemini process.command removed")
for res, a in log_of("gemini_cli.file_operation"):
    check("tool.name" not in a, "tool.name set on gemini_cli.file_operation (would double count tool calls)")

for n, r, a, dp in points:
    if n.startswith("gemini_cli."):
        check("process.command_args" not in r, f"process.command_args kept on metric {n}")

# Qwen Code
recs = log_of("qwen-code.tool_call")
check(len(recs) == 2, f"qwen-code.tool_call records: {len(recs)}")
qtools = {}
for res, a in recs:
    check(res.get("agent.name") == "qwen-code", f"qwen resource agent.name = {res.get('agent.name')}")
    qtools[a.get("tool.name")] = a
    check("function_args" not in a, "qwen function_args not redacted")
    check("error" not in a and "error.message" not in a, "qwen error text not redacted")
check(sorted(qtools) == ["read_file", "run_shell_command"], f"qwen tool.name values = {sorted(qtools, key=str)}")
if "read_file" in qtools:
    check(qtools["read_file"].get("tool.success") is True, f"qwen read_file tool.success = {qtools['read_file'].get('tool.success')}")
    check(qtools["read_file"].get("decision") == "auto_accept", f"qwen read_file decision = {qtools['read_file'].get('decision')}")
if "run_shell_command" in qtools:
    check(qtools["run_shell_command"].get("tool.success") is False, "qwen denied shell call not marked failed")
    check(qtools["run_shell_command"].get("error_type") == "execution_denied", "qwen error_type removed")
for res, a in log_of("qwen-code.file_operation"):
    check("tool.name" not in a, "tool.name set on qwen-code.file_operation (would double count tool calls)")
content = {"qwen-code.user_prompt": "prompt", "qwen-code.api_request": "request_text",
           "qwen-code.api_response": "response_text", "qwen-code.subagent_execution": "result"}
for event, key in content.items():
    recs = log_of(event)
    check(len(recs) == 1, f"{event} records: {len(recs)}")
    for res, a in recs:
        check(key not in a, f"{event} {key} not redacted")
        check("execution_summary" not in a, f"{event} execution_summary not redacted")
for res, a in logs:
    for v in a.values():
        check("secret" not in str(v), f"content left in a log attribute: {v!r}")

qtok = {a.get("type"): dp.get("asInt") for n, r, a, dp in points if n == "qwen-code.token.usage"}
check(qtok == {"input": "6000", "output": "800", "cache": "2000", "thought": "200"}, f"qwen token points = {qtok}")
for n, r, a, dp in points:
    if n.startswith("qwen-code."):
        check(a.get("agent.name") == "qwen-code", f"qwen metric {n} agent.name = {a.get('agent.name')}")
check(any(n == "qwen-code.session.count" for n, r, a, dp in points), "qwen-code.session.count missing")

qspans = [(n, r, a) for n, r, a in spans if r.get("service.name") == "qwen-code"]
check(sorted(n for n, r, a in qspans) == ["qwen-code.interaction", "qwen-code.llm_request", "qwen-code.tool"], f"qwen spans = {sorted(n for n, r, a in qspans)}")
for n, r, a in qspans:
    check(r.get("agent.name") == "qwen-code", f"qwen span {n} agent.name = {r.get('agent.name')}")

# Derived tool histogram
tools = {(a.get("agent.name"), a.get("tool.name")): dp for n, r, a, dp in points if n == "agent_otel_kit.tool.duration"}
check(("codex", "exec_command") in tools, f"derived histogram for codex exec_command missing: {sorted(tools)}")
check(("gemini-cli", "read_file") in tools, f"derived histogram for gemini-cli read_file missing: {sorted(tools)}")
check(("qwen-code", "read_file") in tools, f"derived histogram for qwen-code read_file missing: {sorted(tools)}")
check(("qwen-code", "run_shell_command") in tools, f"derived histogram for qwen-code run_shell_command missing: {sorted(tools)}")
check(len(tools) == 4, f"derived histogram series: {sorted(tools)}")
for k, dp in tools.items():
    check(int(dp.get("count", 0)) == 1, f"derived histogram count for {k} = {dp.get('count')}")
    check(int(dp.get("startTimeUnixNano", "0")) > 0, f"derived histogram start time missing for {k}")

# Claude Code against third-party endpoints: model suffix and cost basis
def sess(n):
    return "00000000-0000-4000-8000-00000000000%d" % n

cost = {a.get("session.id"): (a.get("model"), a.get("cost.basis"), a.get("anthropic.custom_base_url"), dp.get("asDouble"))
        for n, r, a, dp in points if n == "claude_code.cost.usage"}
want = {
    sess(0): ("claude-opus-5-5", "list", False, 0.0081),
    sess(1): ("deepseek-chat", "estimated", True, 0.0081),
    sess(2): ("claude-opus-5-5", "estimated", True, 0.0081),
    sess(3): ("deepseek-flash", "estimated", True, 0.0081),
    sess(4): ("deepseek-flash", "managed", True, 0.0002715),
    sess(5): ("deepseek-flash", "estimated", True, 0.00891),
}
cost = {k: v[:3] + (round(v[3], 12),) for k, v in cost.items()}
check(len([1 for n, r, a, dp in points if n == "claude_code.cost.usage"]) == len(want), "claude cost points not one per session")
check(cost == want, f"claude cost points (model, cost.basis, anthropic.custom_base_url, value) = {cost}")
for n, r, a, dp in points:
    if n.startswith("claude_code."):
        check("[" not in (a.get("model") or ""), f"model suffix kept on {n}: {a.get('model')}")
tok = sorted((a.get("type"), int(dp.get("asInt") or dp.get("asDouble") or 0)) for n, r, a, dp in points
             if n == "claude_code.token.usage" and a.get("session.id") == sess(3))
check(tok == [("cacheCreation", 0), ("cacheRead", 500), ("input", 1000), ("output", 200)], f"claude token points of session 3 = {tok}")
check(all(a.get("model") == "deepseek-flash" for n, r, a, dp in points if n == "claude_code.token.usage" and a.get("session.id") == sess(3)),
      "claude token points of session 3 not normalized to deepseek-flash")
tok = sorted((a.get("model"), a.get("type"), int(dp.get("asInt") or dp.get("asDouble") or 0)) for n, r, a, dp in points
             if n == "claude_code.token.usage" and a.get("session.id") == sess(5))
check(tok == [("deepseek-flash", "cacheCreation", 0), ("deepseek-flash", "cacheRead", 550),
              ("deepseek-flash", "input", 1100), ("deepseek-flash", "output", 220)],
      f"claude token points of session 5 (suffixed and plain model in one export) = {tok}")
check(not any("cost.basis" in a for n, r, a, dp in points if n == "claude_code.token.usage" and a.get("session.id") != sess(4)),
      "cost.basis set on token points")

reqs = {a.get("session.id"): (a.get("model"), a.get("cost.basis")) for r, a in log_of("api_request")}
check(reqs == {sess(1): ("deepseek-chat", "estimated"), sess(2): ("glm-4.6", "estimated"),
               sess(3): ("deepseek-flash", "estimated"), sess(4): ("deepseek-flash", "managed")},
      f"claude api_request (model, cost.basis) = {reqs}")

spans = []
for batch in lines("traces.json"):
    for rs in batch.get("resourceSpans", []):
        for ss in rs.get("scopeSpans", []):
            for sp in ss.get("spans", []):
                spans.append((sp["name"], attrs(sp.get("attributes"))))
llm = {a.get("session.id"): (a.get("model"), a.get("gen_ai.request.model")) for n, a in spans if n == "claude_code.llm_request"}
check(llm == {sess(1): ("deepseek-chat",) * 2, sess(2): ("claude-opus-5-5",) * 2,
              sess(3): ("deepseek-flash",) * 2, sess(4): ("deepseek-flash",) * 2},
      f"claude llm_request span (model, gen_ai.request.model) = {llm}")

if fails:
    print("collector test FAILED:")
    for f in fails:
        print("  -", f)
    sys.exit(1)
print(f"collector test ok: {len(logs)} log records, {len(points)} metric points, {len(spans)} spans checked")
PY

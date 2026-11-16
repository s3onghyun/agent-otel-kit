// Command gendashboards writes the Grafana and Perses dashboards from one
// panel list, so the two stay in step.
//
//	go run ./tools/gendashboards        (from the repository root)
//
// Output: grafana/dashboards/*.json and perses/provisioning/dashboards/*.json.
// CI regenerates them and fails if the committed files differ.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type dsKind string

const (
	prom  dsKind = "prometheus"
	loki  dsKind = "loki"
	tempo dsKind = "tempo"
)

type query struct {
	ds      dsKind
	expr    string
	legend  string // Grafana legendFormat / Perses seriesNameFormat
	instant bool
}

type panelKind string

const (
	stat       panelKind = "stat"
	timeseries panelKind = "timeseries"
	bars       panelKind = "bars"  // horizontal bars, one per series
	table      panelKind = "table" // instant query results as rows
	logs       panelKind = "logs"
	traceTable panelKind = "tracetable"
	traceView  panelKind = "traceview"
)

type column struct {
	name, header, unit string
}

type panel struct {
	id, title, desc string
	kind            panelKind
	unit            string // Grafana unit id; mapped for Perses
	decimals        int
	stacked         bool
	queries         []query
	columns         []column // table: label and value columns to show, in order
	joinOn          []string // table with several queries: join rows on these labels
	x, y, w, h      int
}

type variable struct {
	name, label string
	// Prometheus label values; empty matcher means any series.
	labelName, matcher string
	text               bool // free text (trace ID)
}

type dashboard struct {
	uid, title, desc string
	tags             []string
	vars             []variable
	panels           []panel
}

// rangeTotal is how much a cumulative counter grew inside the dashboard
// time range. The anchored modifier (Prometheus
// --enable-feature=promql-extended-range-selectors, see compose.yaml) takes
// the difference between the last sample and the sample at the start of the
// range, handles counter resets and does not extrapolate like plain
// increase(). Together with the zero sample Prometheus inserts for new
// series (created-timestamp-zero-ingestion), a short session's first export
// is counted too.
func rangeTotal(sel string) string {
	return fmt.Sprintf("increase(%s[$__range] anchored)", sel)
}

// intervalDelta is the same per graph step, so the bars add up to the
// totals in the stat panels.
func intervalDelta(sel string) string {
	return fmt.Sprintf("increase(%s[$__interval] anchored)", sel)
}

const (
	costSel = `claude_code_cost_usage_USD_total{agent_name=~"$agent", model=~"$model", cost_basis=~"$cost_basis"}`
	// The collector labels each cost series with cost_basis: list or
	// managed (exact rates) or estimated (third-party endpoint). The stats
	// and the per-session table keep estimated cost apart; the per-model
	// panels show the basis in each series name.
	costExactSel     = `claude_code_cost_usage_USD_total{agent_name=~"$agent", model=~"$model", cost_basis=~"$cost_basis", cost_basis!="estimated"}`
	costEstimatedSel = `claude_code_cost_usage_USD_total{agent_name=~"$agent", model=~"$model", cost_basis=~"$cost_basis", cost_basis="estimated"}`
	costBasisDesc    = "estimated: third-party endpoint, Claude Code default-rate guess. Claude Code sessions sent to ANTHROPIC_BASE_URL or a gateway (anthropic_custom_base_url=true) are still priced at Anthropic rates: an unknown model ID at the default model's rate, a server-mapped claude-* ID at Claude list price. list: Anthropic list price. managed: your own modelPricing rates (see the README). Token counts are exact either way."
	// Token types that are parts of other types are left out of summed
	// totals (tokenSel) so the sums are exact; per-type breakdowns
	// (tokenAllSel) show every type. Claude Code reports disjoint types (cacheRead is not part
	// of input). Codex reports tokens per turn as a histogram
	// (codex.turn.token_usage, whose _sum is a counter) and counts cached
	// input inside input and reasoning inside output, plus a total. Gemini
	// CLI counts cache inside input; its thought and tool tokens are extra.
	// Qwen Code counts cache inside input and, on OpenAI-compatible
	// providers, thought (reasoning) inside output, so its exact total is
	// input + output and it needs its own selector (qwenTokenSel).
	tokenOverlap  = `type!~"total|cached_input|cache_write_input|reasoning_output|cache"`
	tokenAllSel   = `{__name__=~"claude_code_token_usage_tokens_total|gemini_cli_token_usage_total|codex_turn_token_usage_sum|qwen_code_token_usage_total", agent_name=~"$agent", model=~"$model"}`
	tokenSel      = `{__name__=~"claude_code_token_usage_tokens_total|gemini_cli_token_usage_total|codex_turn_token_usage_sum", agent_name=~"$agent", model=~"$model", ` + tokenOverlap + `}`
	qwenTokenSel  = `qwen_code_token_usage_total{agent_name=~"$agent", model=~"$model", type!~"cache|thought"}`
	tokenTypeDesc = "Codex cached_input, cache_write_input, reasoning_output and total, Gemini CLI cache and Qwen Code cache and thought overlap with input or output and are left out of the Tokens total."
	sessionSel    = `{__name__=~"claude_code_session_count_total|gemini_cli_session_count_total|codex_thread_started_total|qwen_code_session_count_total", agent_name=~"$agent"}`
	toolCount     = `agent_otel_kit_tool_duration_milliseconds_count{agent_name=~"$agent", tool_name=~"$tool"}`
	toolBucket    = `agent_otel_kit_tool_duration_milliseconds_bucket{agent_name=~"$agent", tool_name=~"$tool"}`
	toolFailed    = `agent_otel_kit_tool_duration_milliseconds_count{agent_name=~"$agent", tool_name=~"$tool", tool_success="false"}`
)

// traceRoots are the root spans of the traces worth listing. Codex also
// sends many short internal traces (auth, fs.read_file, ...).
const traceRoots = `claude_code.interaction|turn/start|llm_call|schedule_tool_calls|qwen-code.interaction`

var agentVar = variable{name: "agent", label: "Agent", labelName: "agent_name", matcher: `{agent_name!=""}`}

func dashboards() []dashboard {
	costTokens := dashboard{
		uid:   "agent-cost-tokens",
		title: "Agent cost and tokens",
		desc:  "Cost and token usage per coding agent and model. Cost is reported by Claude Code only; cost of sessions on third-party endpoints is an estimate and shown apart.",
		tags:  []string{"agent-otel-kit"},
		vars: []variable{
			agentVar,
			{name: "model", label: "Model", labelName: "model", matcher: `{agent_name=~"$agent", model!=""}`},
			{name: "cost_basis", label: "Cost basis", labelName: "cost_basis", matcher: `claude_code_cost_usage_USD_total{agent_name=~"$agent"}`},
		},
		panels: []panel{
			{id: "cost_total", title: "Cost (USD)", desc: "Cost reported by Claude Code (claude_code.cost.usage) in the selected range at list price or your managed modelPricing rates. Estimated cost is not included. Codex and Gemini CLI do not export cost.",
				kind: stat, unit: "currencyUSD", decimals: 4, x: 0, y: 0, w: 5, h: 4,
				queries: []query{{ds: prom, expr: "sum(" + rangeTotal(costExactSel) + ") or vector(0)", instant: true}}},
			{id: "cost_estimated_total", title: "Estimated cost (USD)", desc: costBasisDesc,
				kind: stat, unit: "currencyUSD", decimals: 4, x: 5, y: 0, w: 5, h: 4,
				queries: []query{{ds: prom, expr: "sum(" + rangeTotal(costEstimatedSel) + ") or vector(0)", instant: true}}},
			{id: "tokens_total", title: "Tokens", desc: "All tokens in the selected range. Cached input that Codex, Gemini CLI and Qwen Code count inside input, and the reasoning Qwen Code counts inside output, are not added again.",
				kind: stat, unit: "short", x: 10, y: 0, w: 5, h: 4,
				queries: []query{{ds: prom, expr: "sum(" + rangeTotal(tokenSel) + " or " + rangeTotal(qwenTokenSel) + ")", instant: true}}},
			{id: "sessions_total", title: "Sessions", desc: "Sessions started in the selected range (Claude Code, Gemini CLI and Qwen Code sessions, Codex threads).",
				kind: stat, unit: "short", x: 15, y: 0, w: 5, h: 4,
				queries: []query{{ds: prom, expr: "sum(" + rangeTotal(sessionSel) + ")", instant: true}}},
			{id: "active_time", title: "Active time", desc: "claude_code.active_time.total: time the user and the CLI were active.",
				kind: stat, unit: "s", x: 20, y: 0, w: 4, h: 4,
				queries: []query{{ds: prom, expr: "sum(" + rangeTotal(`claude_code_active_time_seconds_total{agent_name=~"$agent"}`) + ")", instant: true}}},

			{id: "cost_rate", title: "Cost per interval by model", desc: costBasisDesc, kind: timeseries, unit: "currencyUSD", decimals: 4, stacked: true, x: 0, y: 4, w: 12, h: 9,
				queries: []query{{ds: prom, expr: `sum by (agent_name, model, cost_basis) (` + intervalDelta(costSel) + `)`, legend: "{{agent_name}} {{model}} ({{cost_basis}})"}}},
			{id: "token_rate", title: "Tokens per interval by type", desc: tokenTypeDesc, kind: timeseries, unit: "short", stacked: true, x: 12, y: 4, w: 12, h: 9,
				queries: []query{{ds: prom, expr: `sum by (agent_name, type) (` + intervalDelta(tokenAllSel) + `)`, legend: "{{agent_name}} {{type}}"}}},

			{id: "cost_by_model", title: "Cost by model", desc: costBasisDesc, kind: bars, unit: "currencyUSD", decimals: 4, x: 0, y: 13, w: 12, h: 8,
				queries: []query{{ds: prom, expr: `sort_desc(sum by (agent_name, model, cost_basis) (` + rangeTotal(costSel) + `))`, legend: "{{agent_name}} {{model}} ({{cost_basis}})", instant: true}}},
			{id: "tokens_by_model", title: "Tokens by agent, model and type", desc: tokenTypeDesc, kind: table, x: 12, y: 13, w: 12, h: 8,
				queries: []query{{ds: prom, expr: `sort_desc(sum by (agent_name, model, type) (` + rangeTotal(tokenAllSel) + `))`, instant: true}},
				columns: []column{{"agent_name", "Agent", ""}, {"model", "Model", ""}, {"type", "Type", ""}, {"value", "Tokens", "short"}}},

			{id: "top_sessions", title: "Most expensive sessions", desc: costBasisDesc, kind: table, x: 0, y: 21, w: 24, h: 8,
				queries: []query{{ds: prom, expr: `topk(10, sum by (agent_name, session_id, model, cost_basis) (` + rangeTotal(costSel) + `))`, instant: true}},
				columns: []column{{"session_id", "Session", ""}, {"agent_name", "Agent", ""}, {"model", "Model", ""}, {"cost_basis", "Cost basis", ""}, {"value", "Cost (USD)", "currencyUSD"}}},
		},
	}

	tools := dashboard{
		uid:   "agent-tool-calls",
		title: "Agent tool calls",
		desc:  "Tool call volume, success rate and duration, from the tool result events of each agent.",
		tags:  []string{"agent-otel-kit"},
		vars: []variable{
			agentVar,
			{name: "tool", label: "Tool", labelName: "tool_name", matcher: `agent_otel_kit_tool_duration_milliseconds_count{agent_name=~"$agent"}`},
		},
		panels: []panel{
			{id: "calls_total", title: "Tool calls", kind: stat, unit: "short", x: 0, y: 0, w: 6, h: 4,
				queries: []query{{ds: prom, expr: "sum(" + rangeTotal(toolCount) + ")", instant: true}}},
			{id: "failed_total", title: "Failed tool calls", kind: stat, unit: "short", x: 6, y: 0, w: 6, h: 4,
				queries: []query{{ds: prom, expr: "sum(" + rangeTotal(toolFailed) + ") or vector(0)", instant: true}}},
			{id: "success_rate", title: "Success rate", kind: stat, unit: "percentunit", decimals: 1, x: 12, y: 0, w: 6, h: 4,
				queries: []query{{ds: prom, expr: "1 - ((sum(" + rangeTotal(toolFailed) + ") or vector(0)) / sum(" + rangeTotal(toolCount) + "))", instant: true}}},
			{id: "p95_total", title: "p95 tool duration", kind: stat, unit: "ms", x: 18, y: 0, w: 6, h: 4,
				queries: []query{{ds: prom, expr: "histogram_quantile(0.95, sum by (le) (" + rangeTotal(toolBucket) + "))", instant: true}}},

			{id: "calls_rate", title: "Tool calls per interval by tool", kind: timeseries, unit: "short", stacked: true, x: 0, y: 4, w: 12, h: 9,
				queries: []query{{ds: prom, expr: `sum by (tool_name) (` + intervalDelta(toolCount) + `)`, legend: "{{tool_name}}"}}},
			{id: "p95_by_tool", title: "p95 duration by tool", kind: timeseries, unit: "ms", x: 12, y: 4, w: 12, h: 9,
				queries: []query{{ds: prom, expr: `histogram_quantile(0.95, sum by (le, tool_name) (rate(` + toolBucket + `[$__rate_interval]))) >= 0`, legend: "{{tool_name}}"}}},

			{id: "per_tool", title: "Per tool", kind: table, x: 0, y: 13, w: 14, h: 9,
				queries: []query{
					{ds: prom, expr: `sort_desc(sum by (agent_name, tool_name) (` + rangeTotal(toolCount) + `))`, instant: true},
					{ds: prom, expr: `sum by (agent_name, tool_name) (` + rangeTotal(toolCount) + `) - sum by (agent_name, tool_name) (` + rangeTotal(toolFailed) + ` or (0 * ` + rangeTotal(toolCount) + `))`, instant: true},
					{ds: prom, expr: `histogram_quantile(0.5, sum by (le, agent_name, tool_name) (` + rangeTotal(toolBucket) + `))`, instant: true},
					{ds: prom, expr: `histogram_quantile(0.95, sum by (le, agent_name, tool_name) (` + rangeTotal(toolBucket) + `))`, instant: true},
				},
				joinOn: []string{"agent_name", "tool_name"},
				columns: []column{{"agent_name", "Agent", ""}, {"tool_name", "Tool", ""},
					{"value #1", "Calls", "short"}, {"value #2", "Succeeded", "short"}, {"value #3", "p50", "ms"}, {"value #4", "p95", "ms"}}},
			{id: "decisions", title: "Permission decisions", desc: "Who allowed or rejected each tool call: Claude Code and Codex tool_decision events (with source), Gemini CLI and Qwen Code tool_call events.",
				kind: table, x: 14, y: 13, w: 10, h: 9,
				queries: []query{{ds: loki, expr: `sum by (tool_name, decision, source) (count_over_time({service_name=~".+", agent_name=~"$agent"} | event_name=~"tool_decision|codex.tool_decision|gemini_cli.tool_call|qwen-code.tool_call" [$__range]))`, instant: true}},
				columns: []column{{"tool_name", "Tool", ""}, {"decision", "Decision", ""}, {"source", "Source", ""}, {"value", "Count", "short"}}},

			{id: "failed_calls", title: "Failed tool calls", desc: "Error text is not collected by default; error_type is.",
				kind: logs, x: 0, y: 22, w: 24, h: 9,
				queries: []query{{ds: loki, expr: `{service_name=~".+", agent_name=~"$agent"} | tool_success="false" | line_format "{{.agent_name}}  {{.tool_name}}  {{.error_type}}  {{.duration_ms}} ms  session={{.session_id}}"`}}},
		},
	}

	sessions := dashboard{
		uid:   "agent-sessions",
		title: "Agent sessions and traces",
		desc:  "Sessions, interaction traces and the event stream of each session.",
		tags:  []string{"agent-otel-kit"},
		vars: []variable{
			agentVar,
			{name: "session", label: "Session", labelName: "session_id", matcher: `{__name__=~"claude_code_session_count_total|gemini_cli_session_count_total|qwen_code_session_count_total", agent_name=~"$agent"}`},
			{name: "trace", label: "Trace ID", text: true},
		},
		panels: []panel{
			{id: "sessions", title: "Sessions", desc: "Lifetime cost and tokens of each session seen in the range. Estimated cost (third-party endpoint, Claude Code default-rate guess) has its own column. Qwen Code sessions are listed only when it adds session.id to metrics (telemetry.metrics.includeSessionId, see README).", kind: table, x: 0, y: 0, w: 12, h: 10,
				queries: []query{
					{ds: prom, expr: `sort_desc(sum by (agent_name, session_id) (max_over_time(claude_code_cost_usage_USD_total{agent_name=~"$agent", session_id=~"$session", cost_basis!="estimated"}[$__range])))`, instant: true},
					{ds: prom, expr: `sum by (agent_name, session_id) (max_over_time(claude_code_cost_usage_USD_total{agent_name=~"$agent", session_id=~"$session", cost_basis="estimated"}[$__range]))`, instant: true},
					{ds: prom, expr: `sum by (agent_name, session_id) (max_over_time({__name__=~"claude_code_token_usage_tokens_total|gemini_cli_token_usage_total", agent_name=~"$agent", session_id=~"$session", ` + tokenOverlap + `}[$__range]) or max_over_time(qwen_code_token_usage_total{agent_name=~"$agent", session_id=~"$session", session_id!="", type!~"cache|thought"}[$__range]))`, instant: true},
				},
				joinOn:  []string{"agent_name", "session_id"},
				columns: []column{{"session_id", "Session", ""}, {"agent_name", "Agent", ""}, {"value #1", "Cost (USD)", "currencyUSD"}, {"value #2", "Estimated (USD)", "currencyUSD"}, {"value #3", "Tokens", "short"}}},
			{id: "traces", title: "Interactions (traces)", desc: "Claude Code, Codex and Qwen Code: one trace per prompt (Claude Code needs CLAUDE_CODE_ENHANCED_TELEMETRY_BETA=1). Gemini CLI: one trace per model call and per tool batch. Click a trace ID to open it.",
				kind: traceTable, x: 12, y: 0, w: 12, h: 10,
				queries: []query{{ds: tempo, expr: `{resource.agent.name=~"$agent" && trace:rootName=~"` + traceRoots + `"}`}}},
			{id: "trace_view", title: "Trace", desc: "Paste a trace ID into the Trace ID box above.", kind: traceView, x: 0, y: 10, w: 24, h: 16,
				queries: []query{{ds: tempo, expr: "$trace"}}},

			{id: "llm_latency", title: "Model request duration p95", kind: timeseries, unit: "ms", x: 0, y: 26, w: 12, h: 8,
				queries: []query{{ds: loki, expr: `quantile_over_time(0.95, {service_name=~".+", agent_name=~"$agent"} | event_name=~"api_request|gemini_cli.api_response|qwen-code.api_response" or (event_name="codex.api_request" and endpoint=~".*/responses") | unwrap duration_ms [$__auto]) by (model)`, legend: "{{model}}"}}},
			{id: "event_rate", title: "Events per interval by type", kind: timeseries, unit: "short", stacked: true, x: 12, y: 26, w: 12, h: 8,
				queries: []query{{ds: loki, expr: `sum by (event_name) (count_over_time({service_name=~".+", agent_name=~"$agent"} [$__auto]))`, legend: "{{event_name}}"}}},

			{id: "session_events", title: "Session events", desc: "Prompt, response and tool content are not collected by default.", kind: logs, x: 0, y: 34, w: 24, h: 10,
				queries: []query{{ds: loki, expr: `{service_name=~".+", agent_name=~"$agent"} | session_id=~"$session" | line_format "{{.event_name}}{{if .tool_name}}  tool={{.tool_name}}{{end}}{{if .tool_success}} success={{.tool_success}}{{end}}{{if and .tool_name .duration_ms}} {{.duration_ms}}ms{{end}}{{if .model}}  model={{.model}}{{end}}{{if .cost_usd}}  cost_usd={{.cost_usd}}{{if .cost_basis}} ({{.cost_basis}}){{end}}{{end}}{{if .input_tokens}}  tokens in/out={{.input_tokens}}/{{.output_tokens}}{{end}}"`}}},
		},
	}
	return []dashboard{costTokens, tools, sessions}
}

// ---------------------------------------------------------------- Grafana

func gDS(k dsKind) map[string]any { return map[string]any{"type": string(k), "uid": string(k)} }

func grafana(d dashboard) map[string]any {
	var panels []any
	for i, p := range d.panels {
		panels = append(panels, grafanaPanel(i+1, p))
	}
	var vars []any
	for _, v := range d.vars {
		if v.text {
			vars = append(vars, map[string]any{"type": "textbox", "name": v.name, "label": v.label, "query": "", "current": map[string]any{"text": "", "value": ""}})
			continue
		}
		q := fmt.Sprintf("label_values(%s, %s)", v.matcher, v.labelName)
		vars = append(vars, map[string]any{
			"type": "query", "name": v.name, "label": v.label,
			"datasource": gDS(prom),
			"query":      map[string]any{"qryType": 1, "query": q, "refId": "PrometheusVariableQueryEditor-VariableQuery"},
			"definition": q, "refresh": 2, "sort": 1,
			"includeAll": true, "multi": true, "allValue": ".*",
			"current": map[string]any{"text": []string{"All"}, "value": []string{"$__all"}},
		})
	}
	return map[string]any{
		"uid": d.uid, "title": d.title, "description": d.desc, "tags": d.tags,
		"editable": true, "graphTooltip": 1, "schemaVersion": 41, "version": 1,
		"time":        map[string]any{"from": "now-6h", "to": "now"},
		"refresh":     "30s",
		"timezone":    "browser",
		"panels":      panels,
		"templating":  map[string]any{"list": vars},
		"annotations": map[string]any{"list": []any{}},
		"links": []any{map[string]any{
			"type": "dashboards", "tags": []string{"agent-otel-kit"}, "asDropdown": false,
			"title": "Agent dashboards", "includeVars": true, "keepTime": true,
		}},
	}
}

func grafanaTargets(p panel) []any {
	var ts []any
	for i, q := range p.queries {
		ref := string(rune('A' + i))
		t := map[string]any{"refId": ref, "datasource": gDS(q.ds)}
		switch q.ds {
		case prom:
			t["expr"] = q.expr
			t["legendFormat"] = q.legend
			if q.instant {
				t["instant"], t["range"] = true, false
				if p.kind == table {
					t["format"] = "table"
				}
			} else {
				t["range"] = true
			}
		case loki:
			t["expr"] = q.expr
			t["legendFormat"] = q.legend
			if q.instant {
				t["queryType"] = "instant"
			} else {
				t["queryType"] = "range"
			}
		case tempo:
			t["queryType"] = "traceql"
			t["query"] = q.expr
			t["limit"] = 20
			t["tableType"] = "traces"
		}
		ts = append(ts, t)
	}
	return ts
}

func grafanaPanel(id int, p panel) map[string]any {
	gp := map[string]any{
		"id": id, "title": p.title, "description": p.desc,
		"gridPos":    map[string]any{"x": p.x, "y": p.y, "w": p.w, "h": p.h},
		"datasource": gDS(p.queries[0].ds),
		"targets":    grafanaTargets(p),
	}
	defaults := map[string]any{}
	if p.unit != "" {
		defaults["unit"] = p.unit
	}
	if p.decimals > 0 {
		defaults["decimals"] = p.decimals
	}
	switch p.kind {
	case stat:
		gp["type"] = "stat"
		defaults["color"] = map[string]any{"mode": "fixed", "fixedColor": "text"}
		gp["options"] = map[string]any{
			"reduceOptions": map[string]any{"calcs": []string{"lastNotNull"}, "fields": "", "values": false},
			"colorMode":     "value", "graphMode": "none", "textMode": "value", "justifyMode": "auto",
		}
	case timeseries:
		gp["type"] = "timeseries"
		gp["interval"] = "1m"
		custom := map[string]any{"drawStyle": "line", "lineWidth": 1, "fillOpacity": 10, "showPoints": "auto", "spanNulls": false}
		if p.stacked {
			custom["drawStyle"] = "bars"
			custom["fillOpacity"] = 80
			custom["stacking"] = map[string]any{"mode": "normal", "group": "A"}
		}
		defaults["custom"] = custom
		gp["options"] = map[string]any{
			"legend":  map[string]any{"displayMode": "table", "placement": "right", "showLegend": true, "calcs": []string{"sum"}},
			"tooltip": map[string]any{"mode": "multi", "sort": "desc"},
		}
		if !p.stacked {
			gp["options"].(map[string]any)["legend"].(map[string]any)["calcs"] = []string{"max"}
		}
	case bars:
		gp["type"] = "bargauge"
		defaults["color"] = map[string]any{"mode": "palette-classic"}
		gp["options"] = map[string]any{
			"orientation": "horizontal", "displayMode": "basic", "valueMode": "color", "showUnfilled": true,
			"namePlacement": "left", "sizing": "manual", "maxVizHeight": 36, "minVizHeight": 16,
			"text":          map[string]any{"titleSize": 13, "valueSize": 14},
			"reduceOptions": map[string]any{"calcs": []string{"lastNotNull"}, "fields": "", "values": false},
		}
	case table:
		gp["type"] = "table"
		gp["options"] = map[string]any{"showHeader": true, "cellHeight": "sm"}
		gp["transformations"] = grafanaTableTransforms(p)
		var overrides []any
		for _, c := range p.columns {
			if c.unit == "" {
				continue
			}
			props := []any{map[string]any{"id": "unit", "value": c.unit}}
			if c.unit == "currencyUSD" {
				props = append(props, map[string]any{"id": "decimals", "value": 4})
			}
			overrides = append(overrides, map[string]any{
				"matcher":    map[string]any{"id": "byName", "options": c.header},
				"properties": props,
			})
		}
		gp["fieldConfig"] = map[string]any{"defaults": defaults, "overrides": overrides}
		return gp
	case logs:
		gp["type"] = "logs"
		gp["options"] = map[string]any{"showTime": true, "wrapLogMessage": true, "enableLogDetails": true, "sortOrder": "Descending", "dedupStrategy": "none"}
	case traceTable:
		gp["type"] = "table"
		gp["options"] = map[string]any{"showHeader": true, "cellHeight": "sm"}
	case traceView:
		gp["type"] = "traces"
	}
	gp["fieldConfig"] = map[string]any{"defaults": defaults, "overrides": []any{}}
	return gp
}

// grafanaTableTransforms turns instant query frames into one table with the
// configured columns, in order, with headers.
func grafanaTableTransforms(p panel) []any {
	var tr []any
	if len(p.queries) > 1 {
		tr = append(tr, map[string]any{"id": "merge", "options": map[string]any{}})
	}
	rename := map[string]any{}
	index := map[string]any{}
	keep := map[string]bool{}
	for i, c := range p.columns {
		src := c.name
		switch {
		case c.name == "value":
			src = "Value"
		case strings.HasPrefix(c.name, "value #"):
			src = "Value #" + string(rune('A'+(c.name[len(c.name)-1]-'1')))
		}
		rename[src] = c.header
		index[src] = i
		keep[src] = true
	}
	exclude := map[string]any{"Time": true, "__name__": true, "job": true, "instance": true}
	tr = append(tr, map[string]any{"id": "organize", "options": map[string]any{
		"excludeByName": exclude, "indexByName": index, "renameByName": rename,
	}})
	tr = append(tr, map[string]any{"id": "filterFieldsByName", "options": map[string]any{
		"include": map[string]any{"names": keysOf(keep)},
	}})
	return tr
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ----------------------------------------------------------------- Perses

func persesFormat(unit string, decimals int) map[string]any {
	f := map[string]any{}
	switch unit {
	case "currencyUSD":
		f["unit"] = "usd"
	case "percentunit":
		f["unit"] = "percent-decimal"
	case "ms":
		f["unit"] = "milliseconds"
	case "s":
		f["unit"] = "seconds"
	case "short", "":
		f["unit"] = "decimal"
		f["shortValues"] = true
	}
	if decimals > 0 {
		f["decimalPlaces"] = decimals
	}
	return f
}

func persesQuery(p panel, q query) map[string]any {
	switch q.ds {
	case loki:
		if p.kind == logs {
			return map[string]any{"kind": "LogQuery", "spec": map[string]any{"plugin": map[string]any{"kind": "LokiLogQuery", "spec": map[string]any{"query": q.expr, "direction": "backward"}}}}
		}
		return map[string]any{"kind": "TimeSeriesQuery", "spec": map[string]any{"plugin": map[string]any{"kind": "LokiTimeSeriesQuery", "spec": map[string]any{"query": persesLoki(q.expr)}}}}
	case tempo:
		return map[string]any{"kind": "TraceQuery", "spec": map[string]any{"plugin": map[string]any{"kind": "TempoTraceQuery", "spec": map[string]any{"query": q.expr, "limit": 20}}}}
	default:
		spec := map[string]any{"query": q.expr}
		if q.legend != "" {
			spec["seriesNameFormat"] = q.legend
		}
		if q.instant {
			spec["instant"] = true
		} else {
			spec["minStep"] = "1m"
		}
		return map[string]any{"kind": "TimeSeriesQuery", "spec": map[string]any{"plugin": map[string]any{"kind": "PrometheusTimeSeriesQuery", "spec": spec}}}
	}
}

// Loki rejects a selector whose only matcher can match the empty string
// (agent_name=~".*" when "All" is picked), hence service_name=~".+".

// persesLoki swaps Grafana-only interval variables for ones Perses knows.
func persesLoki(expr string) string {
	return strings.ReplaceAll(expr, "$__auto", "$__interval")
}

func persesPanel(p panel) map[string]any {
	var plugin map[string]any
	switch p.kind {
	case stat:
		plugin = map[string]any{"kind": "StatChart", "spec": map[string]any{"calculation": "last-number", "format": persesFormat(p.unit, p.decimals)}}
	case timeseries:
		legendCalc := "max"
		if p.stacked {
			legendCalc = "sum"
		}
		visual := map[string]any{"display": "line", "lineWidth": 1.25, "areaOpacity": 0.1}
		if p.stacked {
			visual = map[string]any{"display": "bar", "stack": "all"}
		}
		plugin = map[string]any{"kind": "TimeSeriesChart", "spec": map[string]any{
			"legend": map[string]any{"position": "right", "mode": "table", "values": []string{legendCalc}},
			"yAxis":  map[string]any{"format": persesFormat(p.unit, p.decimals)},
			"visual": visual,
		}}
	case bars:
		plugin = map[string]any{"kind": "BarChart", "spec": map[string]any{"calculation": "last-number", "format": persesFormat(p.unit, p.decimals), "sort": "desc", "mode": "value"}}
	case table:
		var cols []any
		for _, c := range p.columns {
			col := map[string]any{"name": c.name, "header": c.header}
			if c.unit != "" {
				col["format"] = persesFormat(c.unit, map[bool]int{true: 4}[c.unit == "currencyUSD"])
			}
			cols = append(cols, col)
		}
		for _, hide := range []string{"timestamp", "__name__", "job", "instance"} {
			cols = append(cols, map[string]any{"name": hide, "hide": true})
		}
		spec := map[string]any{"density": "compact", "enableSorting": true, "defaultColumnHidden": true, "columnSettings": cols}
		if len(p.joinOn) > 0 {
			// With several queries Perses suffixes label columns with the
			// query number ("agent_name #1"); merge them back, then join rows.
			var tr []any
			for _, l := range p.joinOn {
				var src []string
				for i := range p.queries {
					src = append(src, fmt.Sprintf("%s #%d", l, i+1))
				}
				tr = append(tr, map[string]any{"kind": "MergeColumns", "spec": map[string]any{"columns": src, "name": l}})
			}
			tr = append(tr, map[string]any{"kind": "JoinByColumnValue", "spec": map[string]any{"columns": p.joinOn}})
			spec["transforms"] = tr
		}
		plugin = map[string]any{"kind": "Table", "spec": spec}
	case logs:
		plugin = map[string]any{"kind": "LogsTable", "spec": map[string]any{"showTime": true, "allowWrap": true, "enableDetails": true}}
	case traceTable:
		plugin = map[string]any{"kind": "TraceTable", "spec": map[string]any{}}
	case traceView:
		plugin = map[string]any{"kind": "TracingGanttChart", "spec": map[string]any{}}
	}
	var qs []any
	for _, q := range p.queries {
		qs = append(qs, persesQuery(p, q))
	}
	display := map[string]any{"name": p.title}
	if p.desc != "" {
		display["description"] = p.desc
	}
	return map[string]any{"kind": "Panel", "spec": map[string]any{"display": display, "plugin": plugin, "queries": qs}}
}

func perses(d dashboard) map[string]any {
	panels := map[string]any{}
	var items []any
	for _, p := range d.panels {
		panels[p.id] = persesPanel(p)
		items = append(items, map[string]any{
			"x": p.x, "y": p.y, "width": p.w, "height": p.h,
			"content": map[string]any{"$ref": "#/spec/panels/" + p.id},
		})
	}
	var vars []any
	for _, v := range d.vars {
		if v.text {
			vars = append(vars, map[string]any{"kind": "TextVariable", "spec": map[string]any{"name": v.name, "display": map[string]any{"name": v.label}, "value": ""}})
			continue
		}
		vars = append(vars, map[string]any{"kind": "ListVariable", "spec": map[string]any{
			"name": v.name, "display": map[string]any{"name": v.label},
			"allowAllValue": true, "allowMultiple": true, "customAllValue": ".*", "defaultValue": "$__all",
			"plugin": map[string]any{"kind": "PrometheusLabelValuesVariable", "spec": map[string]any{
				"labelName": v.labelName, "matchers": []string{v.matcher},
			}},
		}})
	}
	return map[string]any{
		"kind":     "Dashboard",
		"metadata": map[string]any{"name": d.uid, "project": "agent-otel-kit"},
		"spec": map[string]any{
			"display":         map[string]any{"name": d.title, "description": d.desc},
			"duration":        "6h",
			"refreshInterval": "30s",
			"variables":       vars,
			"panels":          panels,
			"layouts":         []any{map[string]any{"kind": "Grid", "spec": map[string]any{"items": items}}},
		},
	}
}

// --------------------------------------------------------------------------

func write(path string, v any) error {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, b.Bytes(), 0o644)
}

func main() {
	root := flag.String("root", ".", "repository root")
	flag.Parse()
	for _, d := range dashboards() {
		if err := write(filepath.Join(*root, "grafana", "dashboards", d.uid+".json"), grafana(d)); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if err := write(filepath.Join(*root, "perses", "provisioning", "dashboards", d.uid+".json"), perses(d)); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
}

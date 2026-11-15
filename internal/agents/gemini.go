package agents

import (
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/s3onghyun/agent-otel-kit/internal/jsonobj"
)

// Gemini CLI reads the "telemetry" object of ~/.gemini/settings.json.
// https://github.com/google-gemini/gemini-cli/blob/main/docs/cli/telemetry.md
//
// With otlpProtocol "http" Gemini CLI appends /v1/traces, /v1/logs and
// /v1/metrics to otlpEndpoint itself. logPrompts defaults to true in Gemini
// CLI, so setup turns it off explicitly.
type gemini struct{}

func init() { register(gemini{}) }

func (gemini) Name() string   { return "gemini" }
func (gemini) Binary() string { return "gemini" }

func (gemini) ConfigPath(env Env) string {
	return filepath.Join(env.Home, ".gemini", "settings.json")
}

func (gemini) Apply(current []byte, opts Options) ([]byte, error) {
	root, err := jsonobj.Parse(current)
	if err != nil {
		return nil, fmt.Errorf("settings.json is not plain JSON (comments are not supported by setup; add the telemetry block by hand, see README): %w", err)
	}
	tel, err := root.GetObject("telemetry")
	if err != nil {
		return nil, err
	}
	set := []struct {
		k string
		v any
	}{
		{"enabled", true},
		{"target", "local"},
		{"otlpEndpoint", opts.Endpoint},
		{"otlpProtocol", "http"},
		{"logPrompts", false},
	}
	for _, kv := range set {
		if err := tel.SetValue(kv.k, kv.v); err != nil {
			return nil, err
		}
	}
	// outfile overrides otlpEndpoint; a leftover one would swallow the data.
	tel.Delete("outfile")
	root.SetObject("telemetry", tel)
	return root.Marshal()
}

func (gemini) Check(current []byte, opts Options) Status {
	if current == nil {
		return Status{Detail: "settings file does not exist"}
	}
	root, err := jsonobj.Parse(current)
	if err != nil {
		return Status{Detail: "settings file is not plain JSON: " + err.Error()}
	}
	tel, err := root.GetObject("telemetry")
	if err != nil {
		return Status{Detail: "telemetry is not an object"}
	}
	var enabled bool
	if raw, ok := tel.Get("enabled"); !ok || json.Unmarshal(raw, &enabled) != nil || !enabled {
		return Status{Detail: "telemetry.enabled is not true"}
	}
	if _, ok := tel.Get("outfile"); ok {
		return Status{Detail: "telemetry.outfile is set and overrides otlpEndpoint"}
	}
	ep, _ := tel.GetString("otlpEndpoint")
	if got, err := NormalizeEndpoint(ep); err != nil || got != opts.Endpoint {
		return Status{Detail: fmt.Sprintf("telemetry.otlpEndpoint is %q, want %q", ep, opts.Endpoint)}
	}
	if p, _ := tel.GetString("otlpProtocol"); p != "http" {
		return Status{Detail: fmt.Sprintf("telemetry.otlpProtocol is %q, want \"http\" for %s", p, opts.Endpoint)}
	}
	return Status{Configured: true, Detail: "sends metrics, logs and traces to " + opts.Endpoint}
}

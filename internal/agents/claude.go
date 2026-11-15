package agents

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/s3onghyun/agent-otel-kit/internal/jsonobj"
)

// Claude Code reads telemetry settings from the "env" object of
// ~/.claude/settings.json (or $CLAUDE_CONFIG_DIR/settings.json).
// https://code.claude.com/docs/en/monitoring-usage
type claude struct{}

func init() { register(claude{}) }

func (claude) Name() string   { return "claude" }
func (claude) Binary() string { return "claude" }

func (claude) ConfigPath(env Env) string {
	return filepath.Join(env.dirOr("CLAUDE_CONFIG_DIR", ".claude"), "settings.json")
}

// claudeTraceKeys are only written with Options.Traces.
var claudeTraceKeys = []string{"OTEL_TRACES_EXPORTER", "CLAUDE_CODE_ENHANCED_TELEMETRY_BETA"}

func claudeEnv(opts Options) [][2]string {
	kv := [][2]string{
		{"CLAUDE_CODE_ENABLE_TELEMETRY", "1"},
		{"OTEL_METRICS_EXPORTER", "otlp"},
		{"OTEL_LOGS_EXPORTER", "otlp"},
	}
	if opts.Traces {
		kv = append(kv,
			[2]string{"OTEL_TRACES_EXPORTER", "otlp"},
			[2]string{"CLAUDE_CODE_ENHANCED_TELEMETRY_BETA", "1"},
		)
	}
	return append(kv,
		[2]string{"OTEL_EXPORTER_OTLP_PROTOCOL", "http/protobuf"},
		[2]string{"OTEL_EXPORTER_OTLP_ENDPOINT", opts.Endpoint},
		// The default is 60 s; 10 s makes dashboards follow a session closely.
		[2]string{"OTEL_METRIC_EXPORT_INTERVAL", "10000"},
	)
}

func (claude) Apply(current []byte, opts Options) ([]byte, error) {
	root, err := jsonobj.Parse(current)
	if err != nil {
		return nil, fmt.Errorf("settings.json is not a JSON object: %w", err)
	}
	env, err := root.GetObject("env")
	if err != nil {
		return nil, err
	}
	for _, kv := range claudeEnv(opts) {
		if err := env.SetValue(kv[0], kv[1]); err != nil {
			return nil, err
		}
	}
	if !opts.Traces {
		for _, k := range claudeTraceKeys {
			env.Delete(k)
		}
	}
	root.SetObject("env", env)
	return root.Marshal()
}

func (claude) Check(current []byte, opts Options) Status {
	if current == nil {
		return Status{Detail: "settings file does not exist"}
	}
	root, err := jsonobj.Parse(current)
	if err != nil {
		return Status{Detail: "settings file is not valid JSON: " + err.Error()}
	}
	env, err := root.GetObject("env")
	if err != nil {
		return Status{Detail: "env is not an object"}
	}
	if v, _ := env.GetString("CLAUDE_CODE_ENABLE_TELEMETRY"); v != "1" {
		return Status{Detail: "CLAUDE_CODE_ENABLE_TELEMETRY is not 1"}
	}
	for _, k := range []string{"OTEL_METRICS_EXPORTER", "OTEL_LOGS_EXPORTER"} {
		if v, _ := env.GetString(k); !strings.Contains(v, "otlp") {
			return Status{Detail: k + " does not include otlp"}
		}
	}
	ep, _ := env.GetString("OTEL_EXPORTER_OTLP_ENDPOINT")
	if got, err := NormalizeEndpoint(ep); err != nil || got != opts.Endpoint {
		return Status{Detail: fmt.Sprintf("OTEL_EXPORTER_OTLP_ENDPOINT is %q, want %q", ep, opts.Endpoint)}
	}
	detail := "sends metrics and logs to " + opts.Endpoint
	if v, _ := env.GetString("OTEL_TRACES_EXPORTER"); strings.Contains(v, "otlp") {
		detail = "sends metrics, logs and traces to " + opts.Endpoint
	}
	return Status{Configured: true, Detail: detail}
}

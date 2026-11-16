package agents

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"path/filepath"

	"github.com/s3onghyun/agent-otel-kit/internal/jsonobj"
)

// Qwen Code (a Gemini CLI fork) reads the "telemetry" object of
// ~/.qwen/settings.json.
// https://qwenlm.github.io/qwen-code-docs/en/developers/development/telemetry/
//
// setup prefers otlpProtocol "grpc": Qwen Code's OTLP/HTTP exporter has an
// open report of sending empty bodies (QwenLM/qwen-code#14466). The gRPC
// endpoint is the OTLP/HTTP endpoint with port 4318 replaced by 4317, the two
// ports compose.yaml publishes. Without an explicit protocol, an endpoint on
// any other port gets "http" instead. With "http" Qwen Code appends
// /v1/traces, /v1/logs and /v1/metrics to otlpEndpoint itself.
//
// The per-signal otlp*Endpoint keys and outfile override otlpEndpoint, so
// setup removes them. logPrompts is turned off explicitly. Qwen Code writes
// "$version" into a settings file that lacks it on first start, so a file
// setup creates gets it already; otherwise that rewrite would make
// setup --undo refuse to restore.
type qwen struct{}

func init() { register(qwen{}) }

// qwenSettingsVersion is SETTINGS_VERSION of Qwen Code 0.27
// (packages/cli/src/config/settings.ts).
const qwenSettingsVersion = 4

// qwenOverrides are telemetry keys that send data somewhere other than
// otlpEndpoint when present.
var qwenOverrides = []string{"outfile", "otlpTracesEndpoint", "otlpLogsEndpoint", "otlpMetricsEndpoint"}

func (qwen) Name() string   { return "qwen" }
func (qwen) Binary() string { return "qwen" }

func (qwen) ConfigPath(env Env) string {
	return filepath.Join(env.Home, ".qwen", "settings.json")
}

// QwenGRPCEndpoint maps the collector's OTLP/HTTP endpoint to its OTLP/gRPC
// endpoint: same scheme and host, port 4317 instead of 4318.
func QwenGRPCEndpoint(httpEndpoint string) (string, error) {
	u, err := url.Parse(httpEndpoint)
	if err != nil {
		return "", err
	}
	if u.Port() != "4318" {
		return "", fmt.Errorf("cannot derive an OTLP/gRPC endpoint from %s (only port 4318 maps to 4317); rerun with --protocol http", httpEndpoint)
	}
	return u.Scheme + "://" + net.JoinHostPort(u.Hostname(), "4317"), nil
}

// QwenTarget returns the otlpProtocol and otlpEndpoint setup writes for opts.
// An empty opts.Protocol picks grpc when the endpoint's port is 4318 and
// http otherwise.
func QwenTarget(opts Options) (protocol, endpoint string, err error) {
	switch opts.Protocol {
	case "":
		if ep, err := QwenGRPCEndpoint(opts.Endpoint); err == nil {
			return "grpc", ep, nil
		}
		return "http", opts.Endpoint, nil
	case "grpc":
		ep, err := QwenGRPCEndpoint(opts.Endpoint)
		return "grpc", ep, err
	case "http":
		return "http", opts.Endpoint, nil
	default:
		return "", "", fmt.Errorf("unknown protocol %q (want grpc or http)", opts.Protocol)
	}
}

func (qwen) Apply(current []byte, opts Options) ([]byte, error) {
	protocol, endpoint, err := QwenTarget(opts)
	if err != nil {
		return nil, err
	}
	root, err := jsonobj.Parse(current)
	if err != nil {
		return nil, fmt.Errorf("settings.json is not plain JSON (comments are not supported by setup; add the telemetry block by hand, see README): %w", err)
	}
	if current == nil {
		if err := root.SetValue("$version", qwenSettingsVersion); err != nil {
			return nil, err
		}
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
		{"otlpEndpoint", endpoint},
		{"otlpProtocol", protocol},
		{"logPrompts", false},
	}
	for _, kv := range set {
		if err := tel.SetValue(kv.k, kv.v); err != nil {
			return nil, err
		}
	}
	for _, k := range qwenOverrides {
		tel.Delete(k)
	}
	root.SetObject("telemetry", tel)
	return root.Marshal()
}

// Check accepts either protocol unless opts.Protocol names one: doctor does
// not know which one setup wrote.
func (qwen) Check(current []byte, opts Options) Status {
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
	if t, _ := tel.GetString("target"); t != "local" {
		return Status{Detail: fmt.Sprintf("telemetry.target is %q, want \"local\"", t)}
	}
	if _, ok := tel.Get("outfile"); ok {
		return Status{Detail: "telemetry.outfile is set and overrides otlpEndpoint"}
	}
	// Qwen Code defaults to grpc when otlpProtocol is missing.
	p, _ := tel.GetString("otlpProtocol")
	if p == "" {
		p = "grpc"
	}
	if opts.Protocol != "" && p != opts.Protocol {
		return Status{Detail: fmt.Sprintf("telemetry.otlpProtocol is %q, want %q", p, opts.Protocol)}
	}
	want := opts.Endpoint
	switch p {
	case "grpc":
		if want, err = QwenGRPCEndpoint(opts.Endpoint); err != nil {
			return Status{Detail: "telemetry.otlpProtocol is \"grpc\": " + err.Error()}
		}
	case "http":
		for _, k := range qwenOverrides[1:] {
			if _, ok := tel.Get(k); ok {
				return Status{Detail: fmt.Sprintf("telemetry.%s is set and overrides otlpEndpoint", k)}
			}
		}
	default:
		return Status{Detail: fmt.Sprintf("telemetry.otlpProtocol is %q, want \"grpc\" or \"http\"", p)}
	}
	ep, _ := tel.GetString("otlpEndpoint")
	if qwenEndpointKey(ep, p) != want {
		return Status{Detail: fmt.Sprintf("telemetry.otlpEndpoint is %q, want %q for otlpProtocol %q", ep, want, p)}
	}
	return Status{Configured: true, Detail: fmt.Sprintf("sends metrics, logs and traces to %s (OTLP/%s)", want, p)}
}

// qwenEndpointKey reduces otlpEndpoint to what Qwen Code actually uses, for
// comparison: the origin for gRPC, the normalized base URL for HTTP. An empty
// endpoint means no OTLP export.
func qwenEndpointKey(ep, protocol string) string {
	if ep == "" {
		return ""
	}
	if protocol == "http" {
		got, err := NormalizeEndpoint(ep)
		if err != nil {
			return ""
		}
		return got
	}
	u, err := url.Parse(ep)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

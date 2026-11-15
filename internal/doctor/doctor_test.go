package doctor

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/s3onghyun/agent-otel-kit/internal/agents"
)

const selfMetrics = `# HELP otelcol_receiver_accepted_metric_points Number of points accepted.
otelcol_receiver_accepted_metric_points_total{receiver="otlp",transport="http"} 120
otelcol_receiver_accepted_log_records_total{receiver="otlp",transport="http"} 40
otelcol_receiver_accepted_spans_total{receiver="otlp",transport="http"} 12
otelcol_receiver_refused_spans_total{receiver="otlp",transport="http"} 0
`

func fakeStack(t *testing.T, metrics string, promResult string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	ok := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }
	mux.HandleFunc("/health", ok)
	mux.HandleFunc("/-/ready", ok)
	mux.HandleFunc("/ready", ok)
	mux.HandleFunc("/api/health", ok)
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(metrics)) })
	mux.HandleFunc("/api/v1/query", func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Query().Get("query"), "agent_name") {
			t.Errorf("unexpected query %q", r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[` + promResult + `]}}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func cfgFor(srv *httptest.Server, files map[string]string) Config {
	c := Defaults()
	c.CollectorHealth = srv.URL + "/health"
	c.CollectorMetrics = srv.URL + "/metrics"
	c.Prometheus, c.Loki, c.Tempo, c.Grafana = srv.URL, srv.URL, srv.URL, srv.URL
	c.Perses = "http://127.0.0.1:1" // not running
	c.Env = agents.Env{Home: "/home/u", Getenv: func(string) string { return "" }}
	c.LookPath = func(bin string) (string, error) {
		if bin == "claude" {
			return "/usr/bin/claude", nil
		}
		return "", errors.New("not found")
	}
	c.ReadFile = func(p string) ([]byte, error) {
		if s, ok := files[p]; ok {
			return []byte(s), nil
		}
		return nil, fs.ErrNotExist
	}
	return c
}

func find(t *testing.T, rs []Result, name string) Result {
	t.Helper()
	for _, r := range rs {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("no result %q in %+v", name, rs)
	return Result{}
}

func TestAllGreen(t *testing.T) {
	srv := fakeStack(t, selfMetrics, `{"metric":{"agent_name":"claude-code"},"value":[0,"7"]}`)
	claude, _ := agents.Get("claude")
	settings, _ := claude.Apply(nil, agents.Options{Endpoint: agents.DefaultEndpoint, Traces: true})
	rs := Run(context.Background(), cfgFor(srv, map[string]string{"/home/u/.claude/settings.json": string(settings)}))

	if Failed(rs) {
		var b bytes.Buffer
		Print(&b, rs)
		t.Fatalf("unexpected failure:\n%s", b.String())
	}
	if r := find(t, rs, "collector receiving"); r.Level != OK || !strings.Contains(r.Detail, "spans=12") {
		t.Fatalf("receiving = %+v", r)
	}
	if r := find(t, rs, "agent metrics"); r.Level != OK || !strings.Contains(r.Detail, "claude-code") {
		t.Fatalf("agent metrics = %+v", r)
	}
	if r := find(t, rs, "config claude"); r.Level != OK {
		t.Fatalf("config claude = %+v", r)
	}
	// Not installed and no config: informational only.
	if r := find(t, rs, "config codex"); r.Level != Info {
		t.Fatalf("config codex = %+v", r)
	}
	if r := find(t, rs, "perses"); r.Level != Info {
		t.Fatalf("perses = %+v", r)
	}
}

func TestNoDataAndMisconfiguredAgentWarn(t *testing.T) {
	srv := fakeStack(t, "otelcol_process_uptime 3\n", "")
	files := map[string]string{"/home/u/.claude/settings.json": `{"env":{"CLAUDE_CODE_ENABLE_TELEMETRY":"1","OTEL_METRICS_EXPORTER":"otlp","OTEL_LOGS_EXPORTER":"otlp","OTEL_EXPORTER_OTLP_ENDPOINT":"http://other:4318"}}`}
	rs := Run(context.Background(), cfgFor(srv, files))
	if Failed(rs) {
		t.Fatal("warnings should not fail")
	}
	for _, name := range []string{"collector receiving", "agent metrics", "config claude", "agents"} {
		if r := find(t, rs, name); r.Level != Warn {
			t.Errorf("%s = %+v, want warn", name, r)
		}
	}
	if r := find(t, rs, "config claude"); !strings.Contains(r.Detail, "http://other:4318") {
		t.Errorf("detail does not show the wrong endpoint: %s", r.Detail)
	}
}

func TestStackDownFails(t *testing.T) {
	c := cfgFor(fakeStack(t, selfMetrics, ""), nil)
	c.CollectorHealth = "http://127.0.0.1:1/"
	rs := Run(context.Background(), c)
	if !Failed(rs) {
		t.Fatal("collector down did not fail")
	}
	if r := find(t, rs, "collector health"); r.Level != Fail {
		t.Fatalf("collector health = %+v", r)
	}
}

func TestParseSample(t *testing.T) {
	name, v, ok := parseSample(`otelcol_receiver_accepted_spans_total{receiver="otlp"} 1.5e+01`)
	if !ok || name != "otelcol_receiver_accepted_spans_total" || v != 15 {
		t.Fatalf("got %q %v %v", name, v, ok)
	}
	if _, _, ok := parseSample("garbage"); ok {
		t.Fatal("parsed garbage")
	}
}

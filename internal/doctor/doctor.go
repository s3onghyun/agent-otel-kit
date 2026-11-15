// Package doctor checks that the stack is up, that the collector receives
// telemetry and that each agent's config points at it.
package doctor

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/s3onghyun/agent-otel-kit/internal/agents"
)

// Level of a check result.
type Level string

const (
	OK   Level = "ok"
	Info Level = "info"
	Warn Level = "warn"
	Fail Level = "fail"
)

// Result is one line of doctor output.
type Result struct {
	Level  Level
	Name   string
	Detail string
}

// Config holds the URLs doctor probes. The defaults match compose.yaml.
type Config struct {
	CollectorHealth  string
	CollectorMetrics string
	Prometheus       string
	Loki             string
	Tempo            string
	Grafana          string
	Perses           string
	Endpoint         string
	Env              agents.Env
	Client           *http.Client
	LookPath         func(string) (string, error)
	ReadFile         func(string) ([]byte, error)
}

// Defaults returns the configuration for a stack started by compose.yaml.
func Defaults() Config {
	return Config{
		CollectorHealth:  "http://localhost:13133/",
		CollectorMetrics: "http://localhost:8888/metrics",
		Prometheus:       "http://localhost:9090",
		Loki:             "http://localhost:3100",
		Tempo:            "http://localhost:3200",
		Grafana:          "http://localhost:3000",
		Perses:           "http://localhost:8080",
		Endpoint:         agents.DefaultEndpoint,
	}
}

// Run performs every check.
func Run(ctx context.Context, cfg Config) []Result {
	if cfg.Client == nil {
		cfg.Client = &http.Client{Timeout: 5 * time.Second}
	}
	if cfg.ReadFile == nil {
		cfg.ReadFile = os.ReadFile
	}
	var out []Result
	add := func(r Result) { out = append(out, r) }

	probes := []struct{ name, url string }{
		{"collector health", cfg.CollectorHealth},
		{"prometheus ready", strings.TrimRight(cfg.Prometheus, "/") + "/-/ready"},
		{"loki ready", strings.TrimRight(cfg.Loki, "/") + "/ready"},
		{"tempo ready", strings.TrimRight(cfg.Tempo, "/") + "/ready"},
		{"grafana health", strings.TrimRight(cfg.Grafana, "/") + "/api/health"},
	}
	for _, p := range probes {
		if err := get(ctx, cfg.Client, p.url, nil); err != nil {
			add(Result{Fail, p.name, fmt.Sprintf("%s: %v (is the stack up? make up)", p.url, err)})
		} else {
			add(Result{OK, p.name, p.url})
		}
	}
	persesURL := strings.TrimRight(cfg.Perses, "/") + "/api/v1/health"
	if err := get(ctx, cfg.Client, persesURL, nil); err != nil {
		add(Result{Info, "perses", "not running (optional: docker compose --profile perses up -d)"})
	} else {
		add(Result{OK, "perses health", persesURL})
	}

	add(receiverCheck(ctx, cfg))
	add(prometheusAgents(ctx, cfg))
	out = append(out, agentChecks(cfg)...)
	return out
}

// Failed reports whether any result is a failure.
func Failed(rs []Result) bool {
	for _, r := range rs {
		if r.Level == Fail {
			return true
		}
	}
	return false
}

// Print writes results as aligned text.
func Print(w io.Writer, rs []Result) {
	width := 0
	for _, r := range rs {
		width = max(width, len(r.Name))
	}
	for _, r := range rs {
		fmt.Fprintf(w, "%-4s  %-*s  %s\n", r.Level, width, r.Name, r.Detail)
	}
}

func get(ctx context.Context, c *http.Client, u string, into func(io.Reader) error) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := c.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			return ue.Err
		}
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if into != nil {
		return into(resp.Body)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// receiverCheck reads the collector's own metrics and sums the
// otelcol_receiver_accepted_* and otelcol_receiver_refused_* counters.
func receiverCheck(ctx context.Context, cfg Config) Result {
	accepted := map[string]float64{}
	refused := 0.0
	err := get(ctx, cfg.Client, cfg.CollectorMetrics, func(r io.Reader) error {
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			line := sc.Text()
			if strings.HasPrefix(line, "#") {
				continue
			}
			name, val, ok := parseSample(line)
			if !ok {
				continue
			}
			switch {
			case strings.HasPrefix(name, "otelcol_receiver_accepted_"):
				sig := strings.TrimSuffix(strings.TrimPrefix(name, "otelcol_receiver_accepted_"), "_total")
				accepted[sig] += val
			case strings.HasPrefix(name, "otelcol_receiver_refused_"):
				refused += val
			}
		}
		return sc.Err()
	})
	if err != nil {
		return Result{Fail, "collector receiving", fmt.Sprintf("%s: %v", cfg.CollectorMetrics, err)}
	}
	total := 0.0
	var parts []string
	keys := make([]string, 0, len(accepted))
	for k := range accepted {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		total += accepted[k]
		parts = append(parts, fmt.Sprintf("%s=%.0f", k, accepted[k]))
	}
	if total == 0 {
		return Result{Warn, "collector receiving", "nothing received since the collector started; run an agent session (make demo)"}
	}
	detail := "accepted " + strings.Join(parts, " ")
	if refused > 0 {
		return Result{Warn, "collector receiving", fmt.Sprintf("%s, refused %.0f (memory_limiter or a bad payload; see docker compose logs otel-collector)", detail, refused)}
	}
	return Result{OK, "collector receiving", detail}
}

func parseSample(line string) (string, float64, bool) {
	i := strings.LastIndexByte(line, ' ')
	if i < 0 {
		return "", 0, false
	}
	v, err := strconv.ParseFloat(line[i+1:], 64)
	if err != nil {
		return "", 0, false
	}
	name := line[:i]
	if j := strings.IndexByte(name, '{'); j >= 0 {
		name = name[:j]
	}
	return strings.TrimSpace(name), v, true
}

// prometheusAgents lists agent_name values that have metrics in Prometheus.
func prometheusAgents(ctx context.Context, cfg Config) Result {
	q := url.Values{"query": {`count by (agent_name) ({agent_name!=""})`}}
	u := strings.TrimRight(cfg.Prometheus, "/") + "/api/v1/query?" + q.Encode()
	var body struct {
		Data struct {
			Result []struct {
				Metric map[string]string `json:"metric"`
				Value  []any             `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	err := get(ctx, cfg.Client, u, func(r io.Reader) error { return json.NewDecoder(r).Decode(&body) })
	if err != nil {
		return Result{Fail, "agent metrics", fmt.Sprintf("query Prometheus: %v", err)}
	}
	var names []string
	for _, s := range body.Data.Result {
		names = append(names, s.Metric["agent_name"])
	}
	sort.Strings(names)
	if len(names) == 0 {
		return Result{Warn, "agent metrics", "no agent_name series in Prometheus yet; metrics arrive one export interval after a session starts"}
	}
	return Result{OK, "agent metrics", "series in Prometheus for: " + strings.Join(names, ", ")}
}

func agentChecks(cfg Config) []Result {
	var out []Result
	opts := agents.Options{Endpoint: cfg.Endpoint}
	configured := 0
	for _, name := range agents.Names() {
		a, _ := agents.Get(name)
		path := a.ConfigPath(cfg.Env)
		data, err := cfg.ReadFile(path)
		if errors.Is(err, fs.ErrNotExist) {
			data, err = nil, nil
		}
		label := "config " + name
		if err != nil {
			out = append(out, Result{Warn, label, fmt.Sprintf("%s: %v", path, err)})
			continue
		}
		installed := true
		if cfg.LookPath != nil {
			if _, err := cfg.LookPath(a.Binary()); err != nil {
				installed = false
			}
		}
		st := a.Check(data, opts)
		switch {
		case st.Configured:
			configured++
			out = append(out, Result{OK, label, fmt.Sprintf("%s %s", path, st.Detail)})
		case data == nil && !installed:
			out = append(out, Result{Info, label, fmt.Sprintf("%s not found on PATH and %s does not exist; skipped", a.Binary(), path)})
		default:
			out = append(out, Result{Warn, label, fmt.Sprintf("%s: %s (fix: agent-otel-kit setup %s)", path, st.Detail, name)})
		}
	}
	if cfg.Env.Getenv != nil {
		if v := cfg.Env.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"); v != "" {
			if got, err := agents.NormalizeEndpoint(v); err != nil || got != cfg.Endpoint {
				out = append(out, Result{Warn, "shell environment", fmt.Sprintf("OTEL_EXPORTER_OTLP_ENDPOINT=%s is set in this shell; agents that read the shell environment may send there instead of %s", v, cfg.Endpoint)})
			}
		}
	}
	if configured == 0 {
		out = append(out, Result{Warn, "agents", "no agent is configured; run agent-otel-kit setup claude|codex|gemini"})
	}
	return out
}

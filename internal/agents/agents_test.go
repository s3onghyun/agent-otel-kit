package agents

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite golden files")

var testOpts = Options{Endpoint: "http://localhost:4318", Traces: true}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden")
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test ./internal/agents -update)", err)
	}
	if string(got) != string(want) {
		t.Fatalf("%s mismatch\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
	}
}

func TestApplyGolden(t *testing.T) {
	cases := []struct {
		agent, fixture string
		opts           Options
	}{
		{"claude", "claude_existing.json", testOpts},
		{"claude", "", testOpts},
		{"claude", "claude_existing.json", Options{Endpoint: "http://collector.lan:4318"}},
		{"gemini", "gemini_existing.json", testOpts},
		{"gemini", "", testOpts},
		{"codex", "codex_existing.toml", testOpts},
		{"codex", "codex_no_otel.toml", testOpts},
		{"codex", "codex_crlf.toml", testOpts},
		{"codex", "", testOpts},
		{"qwen", "qwen_existing.json", testOpts},
		{"qwen", "qwen_existing.json", Options{Endpoint: testOpts.Endpoint, Traces: true, Protocol: "http"}},
		{"qwen", "qwen_crlf.json", testOpts},
		{"qwen", "", testOpts},
	}
	for _, c := range cases {
		name := c.agent + "_" + strings.TrimSuffix(strings.TrimSuffix(c.fixture, ".json"), ".toml")
		if c.fixture == "" {
			name = c.agent + "_new"
		}
		if !c.opts.Traces {
			name += "_notraces"
		}
		if c.opts.Protocol != "" {
			name += "_" + c.opts.Protocol
		}
		t.Run(name, func(t *testing.T) {
			a, err := Get(c.agent)
			if err != nil {
				t.Fatal(err)
			}
			var in []byte
			if c.fixture != "" {
				in = readFixture(t, c.fixture)
			}
			if st := a.Check(in, c.opts); st.Configured {
				t.Fatalf("Check before setup = configured (%s)", st.Detail)
			}
			out, err := a.Apply(in, c.opts)
			if err != nil {
				t.Fatal(err)
			}
			golden(t, name, out)

			again, err := a.Apply(out, c.opts)
			if err != nil {
				t.Fatal(err)
			}
			if string(again) != string(out) {
				t.Fatalf("Apply is not idempotent:\n%s", again)
			}
			if st := a.Check(out, c.opts); !st.Configured {
				t.Fatalf("Check after setup = not configured: %s", st.Detail)
			}
			other := Options{Endpoint: "http://elsewhere:4318", Traces: c.opts.Traces}
			if st := a.Check(out, other); st.Configured {
				t.Fatalf("Check with a different endpoint = configured")
			}
		})
	}
}

func TestCodexKeepsUnmanagedOtelKeys(t *testing.T) {
	a, _ := Get("codex")
	out, err := a.Apply(readFixture(t, "codex_existing.toml"), testOpts)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{`environment = "staging"`, "log_sse_deltas = false", `"team" = "platform"`, "[profiles.fast]", "[otel]\nnot a table"} {
		if !strings.Contains(s, want) {
			t.Errorf("output lost %q", want)
		}
	}
	for _, gone := range []string{"otlp-grpc", "x-otlp-meta"} {
		if strings.Contains(s, gone) {
			t.Errorf("output still contains old exporter %q", gone)
		}
	}
	if n := strings.Count(s, "\n[otel]\n"+codexBegin); n != 1 {
		t.Errorf("managed [otel] block count = %d", n)
	}
}

func TestCodexCRLFPreserved(t *testing.T) {
	a, _ := Get("codex")
	out, err := a.Apply(readFixture(t, "codex_crlf.toml"), testOpts)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(out), "\n") != strings.Count(string(out), "\r\n") {
		t.Fatalf("mixed line endings:\n%q", out)
	}
}

func TestCodexRefusesShapesItCannotEditSafely(t *testing.T) {
	a, _ := Get("codex")
	for name, in := range map[string]string{
		"dotted root key":   "otel.environment = \"dev\"\n",
		"inline root table": "otel = { environment = \"dev\" }\n",
		"exporter table":    "[otel]\nenvironment = \"dev\"\n\n[otel.exporter.otlp-http]\nendpoint = \"x\"\n",
		"duplicate otel":    "[otel]\na = 1\n[otel]\nb = 2\n",
	} {
		if _, err := a.Apply([]byte(in), testOpts); err == nil {
			t.Errorf("%s: Apply succeeded, want error", name)
		}
	}
	// A key named otel inside another table is fine.
	if _, err := a.Apply([]byte("[profiles.x]\notel = 1\n"), testOpts); err != nil {
		t.Errorf("otel key inside another table: %v", err)
	}
}

func TestClaudeNoTracesRemovesTraceKeys(t *testing.T) {
	a, _ := Get("claude")
	withTraces, _ := a.Apply(nil, testOpts)
	out, err := a.Apply(withTraces, Options{Endpoint: testOpts.Endpoint})
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range claudeTraceKeys {
		if strings.Contains(string(out), k) {
			t.Errorf("%s still present", k)
		}
	}
}

func TestJSONAgentsRejectInvalidJSON(t *testing.T) {
	for _, name := range []string{"claude", "gemini", "qwen"} {
		a, _ := Get(name)
		if _, err := a.Apply([]byte("{\n  // comment\n  \"a\": 1\n}\n"), testOpts); err == nil {
			t.Errorf("%s: Apply on JSON with comments succeeded", name)
		}
	}
}

func TestConfigPaths(t *testing.T) {
	env := Env{Home: "/h", Getenv: func(string) string { return "" }}
	want := map[string]string{
		"claude": "/h/.claude/settings.json",
		"codex":  "/h/.codex/config.toml",
		"gemini": "/h/.gemini/settings.json",
		"qwen":   "/h/.qwen/settings.json",
	}
	for name, p := range want {
		a, _ := Get(name)
		if got := a.ConfigPath(env); got != p {
			t.Errorf("%s: %s, want %s", name, got, p)
		}
	}
	env.Getenv = func(k string) string {
		return map[string]string{"CLAUDE_CONFIG_DIR": "/c", "CODEX_HOME": "/x"}[k]
	}
	if got := registry["claude"].ConfigPath(env); got != "/c/settings.json" {
		t.Errorf("claude with CLAUDE_CONFIG_DIR: %s", got)
	}
	if got := registry["codex"].ConfigPath(env); got != "/x/config.toml" {
		t.Errorf("codex with CODEX_HOME: %s", got)
	}
}

func TestNormalizeEndpoint(t *testing.T) {
	for in, want := range map[string]string{
		"":                                "http://localhost:4318",
		"http://localhost:4318/":          "http://localhost:4318",
		"http://localhost:4318/v1/logs":   "http://localhost:4318",
		"https://otel.lan/otlp/":          "https://otel.lan/otlp",
		"https://otel.lan/otlp/v1/traces": "https://otel.lan/otlp",
	} {
		got, err := NormalizeEndpoint(in)
		if err != nil || got != want {
			t.Errorf("NormalizeEndpoint(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"localhost:4318", "grpc://x:4317", "http://"} {
		if _, err := NormalizeEndpoint(bad); err == nil {
			t.Errorf("NormalizeEndpoint(%q) succeeded", bad)
		}
	}
}

func TestQwenGRPCEndpoint(t *testing.T) {
	for in, want := range map[string]string{
		"http://localhost:4318":   "http://localhost:4317",
		"https://otel.lan:4318":   "https://otel.lan:4317",
		"http://[::1]:4318":       "http://[::1]:4317",
		"http://127.0.0.1:4318/x": "http://127.0.0.1:4317",
	} {
		got, err := QwenGRPCEndpoint(in)
		if err != nil || got != want {
			t.Errorf("QwenGRPCEndpoint(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	a, _ := Get("qwen")
	for _, ep := range []string{"https://otel.lan/otlp", "http://localhost:14318"} {
		if _, err := a.Apply(nil, Options{Endpoint: ep, Protocol: "grpc"}); err == nil || !strings.Contains(err.Error(), "--protocol http") {
			t.Errorf("Apply with %s over gRPC: err = %v, want a hint to use --protocol http", ep, err)
		}
		if _, err := a.Apply(nil, Options{Endpoint: ep, Protocol: "http"}); err != nil {
			t.Errorf("Apply with %s over HTTP: %v", ep, err)
		}
		if p, got, err := QwenTarget(Options{Endpoint: ep}); err != nil || p != "http" || got != ep {
			t.Errorf("QwenTarget(%s) = %q, %q, %v; want http", ep, p, got, err)
		}
	}
}

func TestQwenNewFileHasSettingsVersion(t *testing.T) {
	// Without $version Qwen Code rewrites the file on first start, and
	// setup --undo would then refuse to restore it.
	a, _ := Get("qwen")
	out, err := a.Apply(nil, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(out), "{\n  \"$version\": 4,\n") {
		t.Fatalf("new file does not start with $version:\n%s", out)
	}
	// An existing file is not given one: a missing $version there means
	// Qwen Code still has to migrate it.
	out, err = a.Apply([]byte(`{"general":{"vimMode":true}}`), testOpts)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "$version") {
		t.Fatalf("$version added to an existing file:\n%s", out)
	}
}

func TestQwenCheck(t *testing.T) {
	a, _ := Get("qwen")
	any := Options{Endpoint: testOpts.Endpoint}
	for name, c := range map[string]struct {
		in         string
		configured bool
	}{
		"grpc":                 {`{"telemetry":{"enabled":true,"target":"local","otlpEndpoint":"http://localhost:4317","otlpProtocol":"grpc"}}`, true},
		"grpc default":         {`{"telemetry":{"enabled":true,"target":"local","otlpEndpoint":"http://localhost:4317"}}`, true},
		"grpc with path":       {`{"telemetry":{"enabled":true,"target":"local","otlpEndpoint":"http://localhost:4317/"}}`, true},
		"http":                 {`{"telemetry":{"enabled":true,"target":"local","otlpEndpoint":"http://localhost:4318/","otlpProtocol":"http"}}`, true},
		"grpc on http port":    {`{"telemetry":{"enabled":true,"target":"local","otlpEndpoint":"http://localhost:4318","otlpProtocol":"grpc"}}`, false},
		"http on grpc port":    {`{"telemetry":{"enabled":true,"target":"local","otlpEndpoint":"http://localhost:4317","otlpProtocol":"http"}}`, false},
		"no endpoint":          {`{"telemetry":{"enabled":true,"target":"local","otlpProtocol":"http"}}`, false},
		"gcp target":           {`{"telemetry":{"enabled":true,"target":"gcp","otlpEndpoint":"http://localhost:4317"}}`, false},
		"disabled":             {`{"telemetry":{"enabled":false,"target":"local","otlpEndpoint":"http://localhost:4317"}}`, false},
		"outfile":              {`{"telemetry":{"enabled":true,"target":"local","otlpEndpoint":"http://localhost:4317","outfile":"t.log"}}`, false},
		"http signal override": {`{"telemetry":{"enabled":true,"target":"local","otlpEndpoint":"http://localhost:4318","otlpProtocol":"http","otlpTracesEndpoint":"http://x/v1/traces"}}`, false},
	} {
		if st := a.Check([]byte(c.in), any); st.Configured != c.configured {
			t.Errorf("%s: Configured = %v (%s), want %v", name, st.Configured, st.Detail, c.configured)
		}
	}
	grpc := `{"telemetry":{"enabled":true,"target":"local","otlpEndpoint":"http://localhost:4317","otlpProtocol":"grpc"}}`
	if st := a.Check([]byte(grpc), Options{Endpoint: testOpts.Endpoint, Protocol: "http"}); st.Configured {
		t.Errorf("Check(grpc config, want http) = configured")
	}
}

func TestQwenCRLFInputIsRewrittenWithLF(t *testing.T) {
	a, _ := Get("qwen")
	out, err := a.Apply(readFixture(t, "qwen_crlf.json"), testOpts)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "\r") {
		t.Fatalf("output keeps CR bytes:\n%q", out)
	}
	if !strings.Contains(string(out), `"theme": "Qwen Dark"`) {
		t.Fatalf("output lost the existing key:\n%s", out)
	}
}

func TestQwenKeepsUnmanagedKeys(t *testing.T) {
	a, _ := Get("qwen")
	out, err := a.Apply(readFixture(t, "qwen_existing.json"), testOpts)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{`"usageStatisticsEnabled": false`, `"selectedType": "openai"`, `"includeSessionId": true`, `"$version": 4`} {
		if !strings.Contains(s, want) {
			t.Errorf("output lost %s", want)
		}
	}
	for _, gone := range []string{"outfile", "otlpLogsEndpoint"} {
		if strings.Contains(s, gone) {
			t.Errorf("output still contains %s", gone)
		}
	}
}

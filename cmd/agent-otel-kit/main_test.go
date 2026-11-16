package main

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/s3onghyun/agent-otel-kit/internal/agents"
)

// testEnv gives each test its own fake $HOME so the real
// ~/.claude/settings.json is never touched.
func testEnv(t *testing.T) agents.Env {
	t.Helper()
	return agents.Env{Home: t.TempDir(), Getenv: func(string) string { return "" }}
}

func runCLI(t *testing.T, env agents.Env, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(args, &out, &errb, env)
	return code, out.String(), errb.String()
}

func writeFile(t *testing.T, path string, data []byte, mode fs.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "internal", "agents", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func assertSame(t *testing.T, path string, want []byte, wantMode fs.FileMode) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s differs from the original:\n%s", path, got)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != wantMode {
		t.Fatalf("%s mode = %v, want %v", path, fi.Mode().Perm(), wantMode)
	}
}

func TestSetupUndoRestoresExistingFilesByteForByte(t *testing.T) {
	cases := map[string]struct {
		agent, rel, fixture, endpoint string
	}{
		"claude":    {"claude", ".claude/settings.json", "claude_existing.json", "localhost:4318"},
		"codex":     {"codex", ".codex/config.toml", "codex_existing.toml", "localhost:4318"},
		"gemini":    {"gemini", ".gemini/settings.json", "gemini_existing.json", "localhost:4318"},
		"qwen":      {"qwen", ".qwen/settings.json", "qwen_existing.json", "localhost:4317"},
		"qwen crlf": {"qwen", ".qwen/settings.json", "qwen_crlf.json", "localhost:4317"},
	}
	for tc, c := range cases {
		name := c.agent
		t.Run(tc, func(t *testing.T) {
			env := testEnv(t)
			path := filepath.Join(env.Home, c.rel)
			orig := fixture(t, c.fixture)
			writeFile(t, path, orig, 0o640)

			code, out, errOut := runCLI(t, env, "setup", name)
			if code != 0 {
				t.Fatalf("setup: %d %s %s", code, out, errOut)
			}
			changed, _ := os.ReadFile(path)
			if bytes.Equal(changed, orig) {
				t.Fatal("setup did not change the file")
			}
			if !strings.Contains(string(changed), c.endpoint) {
				t.Fatalf("endpoint missing:\n%s", changed)
			}
			fi, _ := os.Stat(path)
			if fi.Mode().Perm() != 0o640 {
				t.Fatalf("setup changed the mode to %v", fi.Mode().Perm())
			}

			// A second setup with another endpoint must still undo to the
			// very first original.
			if code, _, e := runCLI(t, env, "setup", name, "--endpoint", "http://127.0.0.1:4318"); code != 0 {
				t.Fatalf("second setup: %s", e)
			}
			code, out, errOut = runCLI(t, env, "setup", "--undo", name)
			if code != 0 {
				t.Fatalf("undo: %d %s %s", code, out, errOut)
			}
			assertSame(t, path, orig, 0o640)

			if code, _, _ := runCLI(t, env, "setup", "--undo", name); code == 0 {
				t.Fatal("second undo succeeded, want nothing-to-undo error")
			}
		})
	}
}

func TestSetupUndoRemovesCreatedFileAndDirs(t *testing.T) {
	env := testEnv(t)
	if code, _, e := runCLI(t, env, "setup", "all"); code != 0 {
		t.Fatalf("setup all: %s", e)
	}
	for _, rel := range []string{".claude/settings.json", ".codex/config.toml", ".gemini/settings.json", ".qwen/settings.json"} {
		fi, err := os.Stat(filepath.Join(env.Home, rel))
		if err != nil {
			t.Fatalf("%s not created: %v", rel, err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %v", rel, fi.Mode().Perm())
		}
	}
	if code, _, e := runCLI(t, env, "setup", "--undo", "all"); code != 0 {
		t.Fatalf("undo all: %s", e)
	}
	entries, _ := os.ReadDir(env.Home)
	for _, e := range entries {
		if e.Name() != ".agent-otel-kit" {
			t.Errorf("left behind %s", e.Name())
		}
	}
}

func TestUndoRefusesAfterHandEditUnlessForced(t *testing.T) {
	env := testEnv(t)
	path := filepath.Join(env.Home, ".claude", "settings.json")
	orig := []byte("{\n  \"model\": \"opus\"\n}\n")
	writeFile(t, path, orig, 0o644)
	if code, _, e := runCLI(t, env, "setup", "claude"); code != 0 {
		t.Fatal(e)
	}
	edited, _ := os.ReadFile(path)
	writeFile(t, path, append(edited, ' '), 0o644)

	code, _, errOut := runCLI(t, env, "setup", "--undo", "claude")
	if code == 0 || !strings.Contains(errOut, "--force") {
		t.Fatalf("undo after edit: code %d, stderr %q", code, errOut)
	}
	if code, _, e := runCLI(t, env, "setup", "--undo", "claude", "--force"); code != 0 {
		t.Fatal(e)
	}
	assertSame(t, path, orig, 0o644)
}

func TestSetupFollowsSymlinkedConfig(t *testing.T) {
	env := testEnv(t)
	target := filepath.Join(env.Home, "dotfiles", "claude-settings.json")
	orig := []byte("{}\n")
	writeFile(t, target, orig, 0o644)
	link := filepath.Join(env.Home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if code, _, e := runCLI(t, env, "setup", "claude"); code != 0 {
		t.Fatal(e)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&fs.ModeSymlink == 0 {
		t.Fatalf("symlink replaced: %v %v", fi, err)
	}
	if code, _, e := runCLI(t, env, "setup", "--undo", "claude"); code != 0 {
		t.Fatal(e)
	}
	assertSame(t, target, orig, 0o644)
}

func TestSetupRejectsBadInput(t *testing.T) {
	env := testEnv(t)
	for _, args := range [][]string{
		{"setup"},
		{"setup", "cursor"},
		{"setup", "claude", "codex"},
		{"setup", "claude", "--endpoint", "localhost:4318"},
		{"setup", "qwen", "--protocol", "grpc+http"},
		{"setup", "qwen", "--protocol", "grpc", "--endpoint", "https://otel.lan/otlp"},
		{"bogus"},
	} {
		if code, _, _ := runCLI(t, env, args...); code == 0 {
			t.Errorf("%v succeeded", args)
		}
	}
	// Invalid JSON is left alone.
	path := filepath.Join(env.Home, ".gemini", "settings.json")
	orig := []byte("{ // comment\n}\n")
	writeFile(t, path, orig, 0o600)
	if code, _, _ := runCLI(t, env, "setup", "gemini"); code == 0 {
		t.Fatal("setup on JSON with comments succeeded")
	}
	assertSame(t, path, orig, 0o600)
}

func TestDryRunWritesNothing(t *testing.T) {
	env := testEnv(t)
	code, out, e := runCLI(t, env, "setup", "codex", "--dry-run")
	if code != 0 {
		t.Fatal(e)
	}
	if !strings.Contains(out, "metrics_exporter") {
		t.Fatalf("dry-run output: %s", out)
	}
	if entries, _ := os.ReadDir(env.Home); len(entries) != 0 {
		t.Fatalf("dry-run wrote files: %v", entries)
	}
}

func TestSetupTwiceIsNoop(t *testing.T) {
	env := testEnv(t)
	runCLI(t, env, "setup", "gemini")
	code, out, _ := runCLI(t, env, "setup", "gemini")
	if code != 0 || !strings.Contains(out, "already points") {
		t.Fatalf("second setup: %d %s", code, out)
	}
}

func TestVersionString(t *testing.T) {
	saved := [3]string{version, commit, date}
	t.Cleanup(func() { version, commit, date = saved[0], saved[1], saved[2] })

	version, commit, date = "0.1.0", "0123456789abcdef0123", "2026-11-15T00:00:00Z"
	if got, want := versionString(nil, false), "agent-otel-kit 0.1.0 (commit 0123456789ab, built 2026-11-15T00:00:00Z)"; got != want {
		t.Errorf("release build: got %q, want %q", got, want)
	}

	version, commit, date = "dev", "", ""
	info := &debug.BuildInfo{
		Main:     debug.Module{Version: "v0.2.0"},
		Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: "fedcba9876543210"}},
	}
	if got, want := versionString(info, true), "agent-otel-kit 0.2.0 (commit fedcba987654)"; got != want {
		t.Errorf("go install build: got %q, want %q", got, want)
	}
	if got, want := versionString(&debug.BuildInfo{Main: debug.Module{Version: "(devel)"}}, true), "agent-otel-kit dev"; got != want {
		t.Errorf("local build: got %q, want %q", got, want)
	}

	code, out, _ := runCLI(t, testEnv(t), "version")
	if code != 0 || !strings.HasPrefix(out, "agent-otel-kit ") {
		t.Errorf("version: code %d, output %q", code, out)
	}
}

func TestSetupQwenProtocol(t *testing.T) {
	env := testEnv(t)
	path := filepath.Join(env.Home, ".qwen", "settings.json")
	if code, _, e := runCLI(t, env, "setup", "qwen", "--protocol", "http", "--endpoint", "https://otel.lan/otlp"); code != 0 {
		t.Fatalf("setup qwen --protocol http: %s", e)
	}
	got, _ := os.ReadFile(path)
	for _, want := range []string{`"otlpProtocol": "http"`, `"otlpEndpoint": "https://otel.lan/otlp"`} {
		if !strings.Contains(string(got), want) {
			t.Errorf("missing %s in:\n%s", want, got)
		}
	}
	if code, _, e := runCLI(t, env, "setup", "qwen"); code != 0 {
		t.Fatalf("setup qwen: %s", e)
	}
	got, _ = os.ReadFile(path)
	for _, want := range []string{`"otlpProtocol": "grpc"`, `"otlpEndpoint": "http://localhost:4317"`} {
		if !strings.Contains(string(got), want) {
			t.Errorf("missing %s in:\n%s", want, got)
		}
	}
	if code, _, e := runCLI(t, env, "setup", "--undo", "qwen"); code != 0 {
		t.Fatalf("undo: %s", e)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("undo left %s: %v", path, err)
	}
}

func TestSetupAllRemoteEndpoint(t *testing.T) {
	env := testEnv(t)
	code, out, e := runCLI(t, env, "setup", "all", "--endpoint", "https://otel.lan/otlp")
	if code != 0 {
		t.Fatalf("setup all: %d %s", code, e)
	}
	for _, name := range []string{"claude", "codex", "gemini", "qwen"} {
		if !strings.Contains(out, name+": wrote ") {
			t.Errorf("%s not configured:\n%s", name, out)
		}
	}
	if !strings.Contains(e, "qwen: using OTLP/HTTP for https://otel.lan/otlp") {
		t.Errorf("missing qwen notice: %q", e)
	}
	var settings struct {
		Telemetry struct{ OtlpEndpoint, OtlpProtocol string }
	}
	raw, _ := os.ReadFile(filepath.Join(env.Home, ".qwen", "settings.json"))
	if err := json.Unmarshal(raw, &settings); err != nil {
		t.Fatal(err)
	}
	if settings.Telemetry.OtlpProtocol != "http" || settings.Telemetry.OtlpEndpoint != "https://otel.lan/otlp" {
		t.Errorf("qwen telemetry = %+v", settings.Telemetry)
	}
}

func TestSetupQwenExplicitGRPCRemoteEndpoint(t *testing.T) {
	env := testEnv(t)
	code, _, e := runCLI(t, env, "setup", "qwen", "--protocol", "grpc", "--endpoint", "https://otel.lan/otlp")
	if code == 0 || !strings.Contains(e, "--protocol http") {
		t.Fatalf("setup qwen --protocol grpc: %d %q", code, e)
	}
	if _, err := os.Stat(filepath.Join(env.Home, ".qwen", "settings.json")); !os.IsNotExist(err) {
		t.Errorf("settings.json written: %v", err)
	}
}

func TestSetupQwenTwiceReportsWrittenEndpoint(t *testing.T) {
	env := testEnv(t)
	runCLI(t, env, "setup", "qwen")
	code, out, _ := runCLI(t, env, "setup", "qwen")
	if code != 0 || !strings.Contains(out, "already points at http://localhost:4317\n") {
		t.Fatalf("second setup qwen: %d %s", code, out)
	}
}

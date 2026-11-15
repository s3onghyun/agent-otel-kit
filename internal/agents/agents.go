// Package agents knows where each coding agent keeps its telemetry settings
// and how to point them at the local collector.
package agents

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Options controls what setup writes.
type Options struct {
	// Endpoint is the OTLP/HTTP base URL of the collector, without a
	// signal path, for example http://localhost:4318.
	Endpoint string
	// Traces enables trace export where it is opt-in (Claude Code beta).
	Traces bool
}

// DefaultEndpoint is the OTLP/HTTP port published by compose.yaml.
const DefaultEndpoint = "http://localhost:4318"

// Status is the result of checking an agent's config.
type Status struct {
	// Configured is true when the config sends telemetry to the endpoint.
	Configured bool
	// Detail explains the state in one line.
	Detail string
}

// Agent edits one agent's config file.
type Agent interface {
	// Name is the CLI name: claude, codex or gemini.
	Name() string
	// Binary is the executable looked up on PATH by doctor.
	Binary() string
	// ConfigPath returns the file setup edits, honouring the agent's own
	// override variables (CLAUDE_CONFIG_DIR, CODEX_HOME).
	ConfigPath(env Env) string
	// Apply returns the new file content given the current content (nil
	// when the file does not exist).
	Apply(current []byte, opts Options) ([]byte, error)
	// Check inspects the current content (nil when missing).
	Check(current []byte, opts Options) Status
}

// Env is the environment and home directory used to locate config files.
type Env struct {
	Home   string
	Getenv func(string) string
}

// OSEnv reads the real environment.
func OSEnv() (Env, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Env{}, err
	}
	return Env{Home: home, Getenv: os.Getenv}, nil
}

func (e Env) get(key string) string {
	if e.Getenv == nil {
		return ""
	}
	return e.Getenv(key)
}

func (e Env) dirOr(envKey string, elems ...string) string {
	if v := e.get(envKey); v != "" {
		return v
	}
	return filepath.Join(append([]string{e.Home}, elems...)...)
}

var registry = map[string]Agent{}

func register(a Agent) { registry[a.Name()] = a }

// Get returns the agent with the given CLI name.
func Get(name string) (Agent, error) {
	a, ok := registry[name]
	if !ok {
		return nil, fmt.Errorf("unknown agent %q (want one of: %s)", name, strings.Join(Names(), ", "))
	}
	return a, nil
}

// Names lists the supported agents.
func Names() []string {
	names := make([]string, 0, len(registry))
	for n := range registry {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// NormalizeEndpoint validates an OTLP/HTTP base URL and strips a trailing
// slash or a signal path such as /v1/logs.
func NormalizeEndpoint(raw string) (string, error) {
	if raw == "" {
		raw = DefaultEndpoint
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("endpoint %q: %w", raw, err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("endpoint %q: want an http:// or https:// URL such as %s", raw, DefaultEndpoint)
	}
	p := strings.TrimRight(u.Path, "/")
	for _, sig := range []string{"/v1/logs", "/v1/metrics", "/v1/traces"} {
		p = strings.TrimSuffix(p, sig)
	}
	u.Path = p
	u.RawQuery, u.Fragment = "", ""
	return u.String(), nil
}

// Command agent-otel-kit points AI coding agents at the local OpenTelemetry
// stack and checks that telemetry arrives.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"

	"github.com/s3onghyun/agent-otel-kit/internal/agents"
	"github.com/s3onghyun/agent-otel-kit/internal/backup"
	"github.com/s3onghyun/agent-otel-kit/internal/doctor"
)

// Set at release build time with -ldflags "-X main.version=... -X main.commit=... -X main.date=...".
var (
	version = "dev"
	commit  = ""
	date    = ""
)

// versionString reports the release version, commit and build date. Builds
// without ldflags (go build, go install) fall back to the Go build info.
func versionString(info *debug.BuildInfo, ok bool) string {
	v, c, d := version, commit, date
	if ok && info != nil {
		if v == "dev" && info.Main.Version != "" && info.Main.Version != "(devel)" {
			v = strings.TrimPrefix(info.Main.Version, "v")
		}
		for _, s := range info.Settings {
			switch {
			case s.Key == "vcs.revision" && c == "":
				c = s.Value
			case s.Key == "vcs.time" && d == "":
				d = s.Value
			}
		}
	}
	if len(c) > 12 {
		c = c[:12]
	}
	out := "agent-otel-kit " + v
	switch {
	case c != "" && d != "":
		out += " (commit " + c + ", built " + d + ")"
	case c != "":
		out += " (commit " + c + ")"
	}
	return out
}

const usage = `agent-otel-kit: send Claude Code, Codex, Gemini CLI and Qwen Code telemetry to a local OpenTelemetry stack.

Usage:
  agent-otel-kit setup <claude|codex|gemini|qwen|all> [--endpoint URL] [--no-traces] [--protocol grpc|http] [--dry-run]
  agent-otel-kit setup --undo <claude|codex|gemini|qwen|all> [--force]
  agent-otel-kit doctor [--endpoint URL] [--prometheus URL] [--grafana URL] ...
  agent-otel-kit version

setup edits only the agent's own user config file:
  claude  ~/.claude/settings.json   ("env" block; $CLAUDE_CONFIG_DIR is honoured)
  codex   ~/.codex/config.toml      ([otel] table; $CODEX_HOME is honoured)
  gemini  ~/.gemini/settings.json   ("telemetry" block)
  qwen    ~/.qwen/settings.json     ("telemetry" block; OTLP/gRPC on port 4317
                                     unless --protocol http)
The original file is saved under ~/.agent-otel-kit/backups and
setup --undo puts it back byte for byte.
`

func main() {
	env, err := agents.OSEnv()
	if err != nil {
		fmt.Fprintln(os.Stderr, "agent-otel-kit:", err)
		os.Exit(1)
	}
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, env))
}

func run(args []string, stdout, stderr io.Writer, env agents.Env) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "setup":
		return runSetup(args[1:], stdout, stderr, env)
	case "doctor":
		return runDoctor(args[1:], stdout, stderr, env)
	case "version", "--version", "-v":
		fmt.Fprintln(stdout, versionString(debug.ReadBuildInfo()))
		return 0
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "agent-otel-kit: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}

// parseInterspersed lets flags appear before or after positional arguments.
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return pos, nil
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

func backupStore(env agents.Env) backup.Store {
	return backup.Store{Dir: filepath.Join(env.Home, ".agent-otel-kit", "backups")}
}

func selectAgents(pos []string) ([]agents.Agent, error) {
	if len(pos) != 1 {
		return nil, fmt.Errorf("want exactly one agent: %s or all", strings.Join(agents.Names(), ", "))
	}
	names := []string{pos[0]}
	if pos[0] == "all" {
		names = agents.Names()
	}
	var out []agents.Agent
	for _, n := range names {
		a, err := agents.Get(n)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, nil
}

func runSetup(args []string, stdout, stderr io.Writer, env agents.Env) int {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	fs.SetOutput(stderr)
	endpoint := fs.String("endpoint", agents.DefaultEndpoint, "OTLP/HTTP base URL of the collector")
	undo := fs.Bool("undo", false, "restore the config file saved by setup")
	force := fs.Bool("force", false, "with --undo: restore even if the file was edited after setup")
	noTraces := fs.Bool("no-traces", false, "do not turn on trace export (Claude Code traces are beta)")
	dryRun := fs.Bool("dry-run", false, "print the new file instead of writing it")
	protocol := fs.String("protocol", "grpc", "OTLP protocol for Qwen Code: grpc (the --endpoint host on port 4317) or http")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return 2
	}
	list, err := selectAgents(pos)
	if err != nil {
		fmt.Fprintln(stderr, "agent-otel-kit setup:", err)
		return 2
	}
	all := len(pos) == 1 && pos[0] == "all"
	store := backupStore(env)

	if *undo {
		code := 0
		for _, a := range list {
			rec, err := store.Undo(a.Name(), *force)
			switch {
			case errors.Is(err, backup.ErrNoRecord):
				if !all {
					fmt.Fprintf(stderr, "%s: nothing to undo (setup has not changed it)\n", a.Name())
					code = 1
				}
			case errors.Is(err, backup.ErrChanged):
				fmt.Fprintf(stderr, "%s: %s was edited after setup; rerun with --force to restore the original anyway (later edits are lost)\n", a.Name(), rec.Path)
				code = 1
			case err != nil:
				fmt.Fprintf(stderr, "%s: %v\n", a.Name(), err)
				code = 1
			case rec.Existed:
				fmt.Fprintf(stdout, "%s: restored %s\n", a.Name(), rec.Path)
			default:
				fmt.Fprintf(stdout, "%s: removed %s (setup had created it)\n", a.Name(), rec.Path)
			}
		}
		return code
	}

	ep, err := agents.NormalizeEndpoint(*endpoint)
	if err != nil {
		fmt.Fprintln(stderr, "agent-otel-kit setup:", err)
		return 2
	}
	if *protocol != "grpc" && *protocol != "http" {
		fmt.Fprintf(stderr, "agent-otel-kit setup: --protocol %q: want grpc or http\n", *protocol)
		return 2
	}
	opts := agents.Options{Endpoint: ep, Traces: !*noTraces, Protocol: *protocol}
	code := 0
	for _, a := range list {
		path := a.ConfigPath(env)
		real, err := backup.ResolvePath(path)
		if err != nil {
			fmt.Fprintf(stderr, "%s: %v\n", a.Name(), err)
			code = 1
			continue
		}
		cur, existed, _, err := backup.ReadCurrent(real)
		if err != nil {
			fmt.Fprintf(stderr, "%s: %v\n", a.Name(), err)
			code = 1
			continue
		}
		if !existed {
			cur = nil
		}
		next, err := a.Apply(cur, opts)
		if err != nil {
			fmt.Fprintf(stderr, "%s: %s: %v\n", a.Name(), path, err)
			code = 1
			continue
		}
		if *dryRun {
			fmt.Fprintf(stdout, "# %s: %s\n%s", a.Name(), path, next)
			continue
		}
		if existed && string(next) == string(cur) {
			fmt.Fprintf(stdout, "%s: %s already points at %s\n", a.Name(), path, ep)
			continue
		}
		if err := store.Apply(a.Name(), path, next); err != nil {
			fmt.Fprintf(stderr, "%s: %v\n", a.Name(), err)
			code = 1
			continue
		}
		fmt.Fprintf(stdout, "%s: wrote %s (undo: agent-otel-kit setup --undo %s)\n", a.Name(), path, a.Name())
	}
	if code == 0 && !*dryRun {
		fmt.Fprintln(stdout, "Start a new agent session, then run: agent-otel-kit doctor")
	}
	return code
}

func runDoctor(args []string, stdout, stderr io.Writer, env agents.Env) int {
	cfg := doctor.Defaults()
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&cfg.Endpoint, "endpoint", cfg.Endpoint, "OTLP/HTTP endpoint agents should point at")
	fs.StringVar(&cfg.CollectorHealth, "collector-health", cfg.CollectorHealth, "collector health_check URL")
	fs.StringVar(&cfg.CollectorMetrics, "collector-metrics", cfg.CollectorMetrics, "collector self-metrics URL")
	fs.StringVar(&cfg.Prometheus, "prometheus", cfg.Prometheus, "Prometheus URL")
	fs.StringVar(&cfg.Loki, "loki", cfg.Loki, "Loki URL")
	fs.StringVar(&cfg.Tempo, "tempo", cfg.Tempo, "Tempo URL")
	fs.StringVar(&cfg.Grafana, "grafana", cfg.Grafana, "Grafana URL")
	fs.StringVar(&cfg.Perses, "perses", cfg.Perses, "Perses URL (optional)")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return 2
	}
	if len(pos) > 0 {
		fmt.Fprintf(stderr, "agent-otel-kit doctor: unexpected argument %q\n", pos[0])
		return 2
	}
	ep, err := agents.NormalizeEndpoint(cfg.Endpoint)
	if err != nil {
		fmt.Fprintln(stderr, "agent-otel-kit doctor:", err)
		return 2
	}
	cfg.Endpoint = ep
	cfg.Env = env
	cfg.LookPath = exec.LookPath
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	rs := doctor.Run(ctx, cfg)
	doctor.Print(stdout, rs)
	if doctor.Failed(rs) {
		return 1
	}
	return 0
}

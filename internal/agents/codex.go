package agents

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// Codex reads the [otel] table of ~/.codex/config.toml (or
// $CODEX_HOME/config.toml). Logs, traces and metrics each have their own
// exporter key; with otlp-http the endpoint is the full signal URL.
// https://learn.chatgpt.com/docs/config-file/config-advanced
//
// The file is edited as text so comments and formatting elsewhere survive.
// Only the keys below are replaced, inside a marked block under [otel].
type codex struct{}

func init() { register(codex{}) }

func (codex) Name() string   { return "codex" }
func (codex) Binary() string { return "codex" }

func (codex) ConfigPath(env Env) string {
	return filepath.Join(env.dirOr("CODEX_HOME", ".codex"), "config.toml")
}

const (
	codexBegin = "# agent-otel-kit: begin (remove with `agent-otel-kit setup --undo codex`)"
	codexEnd   = "# agent-otel-kit: end"
)

var codexManagedKeys = []string{"exporter", "trace_exporter", "metrics_exporter", "log_user_prompt"}

func codexBlock(opts Options) []string {
	exp := func(key, signal string) string {
		return fmt.Sprintf(`%s = { otlp-http = { endpoint = %q, protocol = "binary" } }`, key, opts.Endpoint+"/v1/"+signal)
	}
	lines := []string{codexBegin, exp("exporter", "logs")}
	if opts.Traces {
		lines = append(lines, exp("trace_exporter", "traces"))
	}
	return append(lines, exp("metrics_exporter", "metrics"), "log_user_prompt = false", codexEnd)
}

type tomlLine struct {
	text     string // without the line ending
	header   string // table name for [x] / [[x]] lines, else ""
	inString bool   // line starts inside a multi-line string
}

var headerRE = regexp.MustCompile(`^\s*\[\[?\s*([^\]]+?)\s*\]\]?\s*(#.*)?$`)

func scanTOML(data string) ([]tomlLine, string) {
	eol := "\n"
	if strings.Contains(data, "\r\n") {
		eol = "\r\n"
	}
	data = strings.TrimSuffix(strings.ReplaceAll(data, "\r\n", "\n"), "\n")
	if data == "" {
		return nil, eol
	}
	var out []tomlLine
	var delim string // open multi-line string delimiter
	for _, l := range strings.Split(data, "\n") {
		tl := tomlLine{text: l, inString: delim != ""}
		if delim == "" {
			if m := headerRE.FindStringSubmatch(l); m != nil {
				tl.header = strings.ReplaceAll(m[1], " ", "")
			}
		}
		// Track """ and ''' so a "[" inside a multi-line string is not
		// mistaken for a table header.
		rest := l
		for {
			if delim == "" {
				i, j := strings.Index(rest, `"""`), strings.Index(rest, `'''`)
				if i < 0 && j < 0 {
					break
				}
				if j < 0 || (i >= 0 && i < j) {
					delim, rest = `"""`, rest[i+3:]
				} else {
					delim, rest = `'''`, rest[j+3:]
				}
			} else {
				k := strings.Index(rest, delim)
				if k < 0 {
					break
				}
				delim, rest = "", rest[k+3:]
			}
		}
		out = append(out, tl)
	}
	return out, eol
}

func isKeyLine(text, key string) bool {
	t := strings.TrimSpace(text)
	for _, k := range []string{key, `"` + key + `"`, `'` + key + `'`} {
		if rest, ok := strings.CutPrefix(t, k); ok {
			if strings.HasPrefix(strings.TrimSpace(rest), "=") {
				return true
			}
		}
	}
	return false
}

// bracketDelta counts unbalanced { and [ outside of strings and comments, so
// a multi-line inline table value can be skipped as a whole.
func bracketDelta(s string) int {
	depth := 0
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == '\\' && quote == '"' {
				i++
			} else if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '#':
			return depth
		case c == '{' || c == '[':
			depth++
		case c == '}' || c == ']':
			depth--
		}
	}
	return depth
}

func otelSection(lines []tomlLine) (start, end int, err error) {
	start, end = -1, len(lines)
	seenHeader := false
	for i, l := range lines {
		if l.inString {
			continue
		}
		if !seenHeader && l.header == "" && (isKeyLine(l.text, "otel") || strings.HasPrefix(strings.TrimSpace(l.text), "otel.")) {
			return 0, 0, fmt.Errorf("line %d sets otel outside an [otel] table; move it under [otel] or edit by hand", i+1)
		}
		if l.header == "" {
			continue
		}
		seenHeader = true
		for _, k := range []string{"exporter", "trace_exporter", "metrics_exporter"} {
			if l.header == "otel."+k || strings.HasPrefix(l.header, "otel."+k+".") {
				return 0, 0, fmt.Errorf("line %d defines [%s] as a table; setup writes otel.%s inline, edit by hand", i+1, l.header, k)
			}
		}
		if start >= 0 && end == len(lines) {
			end = i
		}
		if l.header == "otel" {
			if start >= 0 {
				return 0, 0, fmt.Errorf("[otel] appears twice (line %d)", i+1)
			}
			start, end = i, len(lines)
		}
	}
	return start, end, nil
}

func (codex) Apply(current []byte, opts Options) ([]byte, error) {
	lines, eol := scanTOML(string(current))
	start, end, err := otelSection(lines)
	if err != nil {
		return nil, fmt.Errorf("config.toml: %w", err)
	}
	block := codexBlock(opts)
	var out []string
	for _, l := range lines {
		out = append(out, l.text)
	}
	if start < 0 {
		if len(out) > 0 && strings.TrimSpace(out[len(out)-1]) != "" {
			out = append(out, "")
		}
		out = append(out, "[otel]")
		out = append(out, block...)
	} else {
		var body []string
		inBlock := false
		skipDepth := 0
		for _, l := range lines[start+1 : end] {
			t := strings.TrimSpace(l.text)
			switch {
			case skipDepth > 0:
				skipDepth += bracketDelta(l.text)
				continue
			case t == codexBegin || strings.HasPrefix(t, "# agent-otel-kit: begin"):
				inBlock = true
				continue
			case t == codexEnd:
				inBlock = false
				continue
			case inBlock:
				continue
			}
			managed := false
			for _, k := range codexManagedKeys {
				if !l.inString && isKeyLine(l.text, k) {
					managed = true
					skipDepth = bracketDelta(l.text)
					break
				}
			}
			if !managed {
				body = append(body, l.text)
			}
		}
		section := append([]string{lines[start].text}, block...)
		section = append(section, body...)
		out = append(append(append([]string{}, out[:start]...), section...), out[end:]...)
	}
	return []byte(strings.Join(out, eol) + eol), nil
}

var endpointRE = regexp.MustCompile(`endpoint\s*=\s*"([^"]*)"`)

func (codex) Check(current []byte, opts Options) Status {
	if current == nil {
		return Status{Detail: "config.toml does not exist"}
	}
	lines, _ := scanTOML(string(current))
	start, end, err := otelSection(lines)
	if err != nil {
		return Status{Detail: err.Error()}
	}
	if start < 0 {
		return Status{Detail: "no [otel] table"}
	}
	values := map[string]string{}
	key := ""
	depth := 0
	for _, l := range lines[start+1 : end] {
		if depth > 0 {
			values[key] += l.text
			depth += bracketDelta(l.text)
			continue
		}
		for _, k := range []string{"exporter", "trace_exporter", "metrics_exporter"} {
			if !l.inString && isKeyLine(l.text, k) {
				key = k
				values[k] = l.text
				depth = bracketDelta(l.text)
			}
		}
	}
	want := map[string]string{"exporter": "logs", "metrics_exporter": "metrics"}
	for k, sig := range want {
		m := endpointRE.FindStringSubmatch(values[k])
		if m == nil || m[1] != opts.Endpoint+"/v1/"+sig {
			got := ""
			if m != nil {
				got = m[1]
			}
			return Status{Detail: fmt.Sprintf("otel.%s endpoint is %q, want %q", k, got, opts.Endpoint+"/v1/"+sig)}
		}
	}
	detail := "sends logs and metrics to " + opts.Endpoint
	if m := endpointRE.FindStringSubmatch(values["trace_exporter"]); m != nil && m[1] == opts.Endpoint+"/v1/traces" {
		detail = "sends logs, metrics and traces to " + opts.Endpoint
	}
	return Status{Configured: true, Detail: detail}
}

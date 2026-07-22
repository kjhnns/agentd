// Package config parses agentd's single config.toml (design 3.10: one config
// file, nothing coordinates through a shared global settings file).
//
// It implements a small, dependency-free subset of TOML sufficient for our
// schema: a [server] table, [[harness]] and [[channel]] arrays-of-tables, and
// string / bool / int / string-array values. This keeps agentd a pure-stdlib
// single static binary (design 3.11) with no third-party parser to vendor.
package config

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Server is the [server] table.
type Server struct {
	Bind      string // e.g. "127.0.0.1:8787"
	APIBearer string // bearer token gating the local API
	StateDir  string // durable state dir (run-log, etc.)
}

// Harness is one [[harness]] entry.
type Harness struct {
	Kind            string // "claude-code", "codex", ...
	Model           string // per-session default model (optional)
	Cwd             string // default working dir for sessions
	SkipPermissions bool   // bypass the harness's own tool-approval prompts
}

// Workspace is the [workspace] table: where agent workspaces live and which
// one sessions use by default. See internal/workspace.
type Workspace struct {
	Root          string // dir holding workspaces; default ~/.agentd/workspaces
	Default       string // workspace name used when a session names none; default "default"
	GitAutocommit bool   // commit workspace changes after each completed turn (default true)
	Remote        string // optional git remote for MANUAL opt-in push; never pushed automatically
}

// Channel is one [[channel]] entry.
type Channel struct {
	Kind   string   // "telegram", ...
	Token  string   // literal, or "env:VAR" (resolve with ResolveToken)
	Allow  []string // chat-id allowlist, server-enforced
	Policy string   // e.g. "dm-only"
}

// Config is the whole parsed file.
type Config struct {
	Server    Server
	Workspace Workspace
	Harness   []Harness
	Channel   []Channel
}

// ResolveToken expands an "env:VAR" reference to the environment variable's
// value; a literal token is returned unchanged. Empty is returned (no error) so
// the caller can decide whether an absent token is fatal for its channel.
func ResolveToken(token string) string {
	if strings.HasPrefix(token, "env:") {
		return os.Getenv(strings.TrimPrefix(token, "env:"))
	}
	return token
}

// Load reads and parses a config file.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

// Parse parses config bytes.
func Parse(data []byte) (*Config, error) {
	cfg := &Config{
		Server:    Server{Bind: "127.0.0.1:8787", StateDir: "state"},
		Workspace: Workspace{GitAutocommit: true}, // Root/Default resolved by workspace.NewStore
	}
	section := "" // "server" or "" (top-level)
	var curHarness *Harness
	var curChannel *Channel

	sc := bufio.NewScanner(strings.NewReader(string(data)))
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Array-of-tables header: [[name]]
		if strings.HasPrefix(line, "[[") && strings.HasSuffix(line, "]]") {
			name := strings.TrimSpace(line[2 : len(line)-2])
			switch name {
			case "harness":
				cfg.Harness = append(cfg.Harness, Harness{})
				curHarness = &cfg.Harness[len(cfg.Harness)-1]
				curChannel = nil
				section = "harness"
			case "channel":
				cfg.Channel = append(cfg.Channel, Channel{})
				curChannel = &cfg.Channel[len(cfg.Channel)-1]
				curHarness = nil
				section = "channel"
			default:
				return nil, fmt.Errorf("config line %d: unknown array table [[%s]]", lineNo, name)
			}
			continue
		}
		// Table header: [name]
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.TrimSpace(line[1 : len(line)-1])
			curHarness = nil
			curChannel = nil
			continue
		}
		// key = value
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			return nil, fmt.Errorf("config line %d: expected key = value, got %q", lineNo, line)
		}
		key := strings.TrimSpace(line[:eq])
		raw := strings.TrimSpace(line[eq+1:])
		if err := assign(cfg, section, curHarness, curChannel, key, raw); err != nil {
			return nil, fmt.Errorf("config line %d: %w", lineNo, err)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func assign(cfg *Config, section string, h *Harness, ch *Channel, key, raw string) error {
	switch section {
	case "server":
		s, err := asString(raw)
		if err != nil {
			return err
		}
		switch key {
		case "bind":
			cfg.Server.Bind = s
		case "api_bearer":
			cfg.Server.APIBearer = s
		case "state_dir":
			cfg.Server.StateDir = s
		default:
			return fmt.Errorf("unknown [server] key %q", key)
		}
	case "workspace":
		if key == "git_autocommit" {
			b, err := asBool(raw)
			if err != nil {
				return fmt.Errorf("git_autocommit: %w", err)
			}
			cfg.Workspace.GitAutocommit = b
			return nil
		}
		s, err := asString(raw)
		if err != nil {
			return err
		}
		switch key {
		case "root":
			cfg.Workspace.Root = s
		case "default":
			cfg.Workspace.Default = s
		case "remote":
			cfg.Workspace.Remote = s
		default:
			return fmt.Errorf("unknown [workspace] key %q", key)
		}
	case "harness":
		if key == "skip_permissions" {
			b, err := asBool(raw)
			if err != nil {
				return fmt.Errorf("skip_permissions: %w", err)
			}
			h.SkipPermissions = b
			return nil
		}
		s, err := asString(raw)
		if err != nil {
			return err
		}
		switch key {
		case "kind":
			h.Kind = s
		case "model":
			h.Model = s
		case "cwd":
			h.Cwd = s
		default:
			return fmt.Errorf("unknown [[harness]] key %q", key)
		}
	case "channel":
		switch key {
		case "kind":
			s, err := asString(raw)
			if err != nil {
				return err
			}
			ch.Kind = s
		case "token":
			s, err := asString(raw)
			if err != nil {
				return err
			}
			ch.Token = s
		case "policy":
			s, err := asString(raw)
			if err != nil {
				return err
			}
			ch.Policy = s
		case "allow":
			arr, err := asStringArray(raw)
			if err != nil {
				return err
			}
			ch.Allow = arr
		default:
			return fmt.Errorf("unknown [[channel]] key %q", key)
		}
	default:
		return fmt.Errorf("key %q outside a known section", key)
	}
	return nil
}

// stripComment removes a trailing "# ..." comment that is not inside a string.
func stripComment(raw string) string {
	inStr := false
	for i := 0; i < len(raw); i++ {
		switch raw[i] {
		case '"':
			inStr = !inStr
		case '#':
			if !inStr {
				return strings.TrimSpace(raw[:i])
			}
		}
	}
	return strings.TrimSpace(raw)
}

func asString(raw string) (string, error) {
	raw = stripComment(raw)
	if len(raw) >= 2 && raw[0] == '"' && raw[len(raw)-1] == '"' {
		return raw[1 : len(raw)-1], nil
	}
	// tolerate bare booleans/ints coerced to string context? No: require quotes.
	return "", fmt.Errorf("expected quoted string, got %q", raw)
}

func asStringArray(raw string) ([]string, error) {
	raw = stripComment(raw)
	if len(raw) < 2 || raw[0] != '[' || raw[len(raw)-1] != ']' {
		return nil, fmt.Errorf("expected [ ... ] array, got %q", raw)
	}
	inner := strings.TrimSpace(raw[1 : len(raw)-1])
	if inner == "" {
		return nil, nil
	}
	var out []string
	for _, part := range strings.Split(inner, ",") {
		p := strings.TrimSpace(part)
		if p == "" {
			continue
		}
		if len(p) >= 2 && p[0] == '"' && p[len(p)-1] == '"' {
			out = append(out, p[1:len(p)-1])
		} else {
			return nil, fmt.Errorf("array element not a quoted string: %q", p)
		}
	}
	return out, nil
}

// asInt is available for future numeric keys (kept for the schema's growth).
func asInt(raw string) (int64, error) {
	return strconv.ParseInt(stripComment(raw), 10, 64)
}

// asBool is available for future boolean keys.
func asBool(raw string) (bool, error) {
	return strconv.ParseBool(stripComment(raw))
}

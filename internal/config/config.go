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
	"time"
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

// Session is the [session] table: the SESSION LIFECYCLE + CONTEXT-RESET tuning.
// agentd keeps ONE warm session per workspace and, instead of compacting, owns
// a controlled RESET (checkpoint-flush -> teardown -> fresh re-hydrated process)
// when a threshold trips. These are the config-tunable triggers + backstops.
type Session struct {
	ContextResetPressure float64       // reset when context pressure (0..1) crosses this after a turn (default 0.75)
	IdleTimeout          time.Duration // a session idle this long is checkpoint-flushed + reclaimed (default 30m)
	MaxTurns             int           // hard backstop: force a reset after this many turns (0 = off; default 200)
	MaxWallclock         time.Duration // hard backstop: force a reset after this session age (0 = off; default 8h)
	ContextWindow        int           // model context window (tokens) for real pressure (default 200000)
	GCInterval           time.Duration // how often the sweeper enforces idle/dead reclaim (default 1m)
	CheckpointTimeout    time.Duration // bound on the checkpoint-flush turn before teardown proceeds anyway (default 120s)
}

// Channel is one [[channel]] entry.
type Channel struct {
	Kind    string   // "telegram", "web", ...
	Token   string   // literal, or "env:VAR" (resolve with ResolveToken)
	Allow   []string // chat-id allowlist, server-enforced
	Policy  string   // e.g. "dm-only"
	Enabled bool     // default true; enabled=false parks the channel
	Path    string   // web channel: UI route (default "/ui")
	Title   string   // web channel: UI title
}

// Job is one [[job]] entry: a declaratively configured proactive job for the
// scheduler (internal/scheduler). A job = a trigger + a target workspace + a
// task prompt; a job run is an ordinary harness turn on the workspace's warm
// session.
type Job struct {
	Name      string        // unique job name (also its id in the API/CLI)
	Enabled   bool          // default true; enabled = false parks the job
	Trigger   string        // "schedule" (default) | "file" | "webhook"
	Schedule  string        // schedule spec: "@every 30m" | "@daily 07:00" | "M H DOM MON DOW"
	TZ        string        // IANA tz for @daily/cron (default: server local time)
	Workspace string        // target workspace ("" = the default workspace)
	Prompt    string        // the task told to the harness
	Notify    string        // "issues" (default) | "always" | "never"
	Path      string        // watched path (trigger = "file")
	Poll      time.Duration // file poll interval (default 10s)
}

// Media is the [media] table: the CORE multimodal media capability
// (internal/media). Channel-agnostic by design; channels only acquire bytes.
type Media struct {
	Enabled      bool          // default true; false parks all media handling
	WhisperKey   string        // OpenAI API key: literal or "env:VAR" (ResolveToken)
	WhisperModel string        // default "whisper-1"
	WhisperLang  string        // optional ISO-639-1 hint; "" = auto-detect
	MaxFileMB    int           // per-file cap in MB (default 20)
	Retention    time.Duration // media files older than this are swept (default 168h)
	Dir          string        // storage override; empty = <default workspace>/work/media
}

// DefaultMedia returns the built-in media settings applied when [media] is
// absent or partial.
func DefaultMedia() Media {
	return Media{
		Enabled:      true,
		WhisperModel: "whisper-1",
		MaxFileMB:    20,
		Retention:    168 * time.Hour,
	}
}

// Config is the whole parsed file.
type Config struct {
	Server    Server
	Workspace Workspace
	Session   Session
	Media     Media
	Harness   []Harness
	Channel   []Channel
	Job       []Job
}

// DefaultSession returns the built-in session-lifecycle tuning applied when
// [session] is absent or only partially specified.
func DefaultSession() Session {
	return Session{
		ContextResetPressure: 0.75,
		IdleTimeout:          30 * time.Minute,
		MaxTurns:             200,
		MaxWallclock:         8 * time.Hour,
		ContextWindow:        200000,
		GCInterval:           time.Minute,
		CheckpointTimeout:    120 * time.Second,
	}
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
		Session:   DefaultSession(),
		Media:     DefaultMedia(),
	}
	section := "" // "server" or "" (top-level)
	var curHarness *Harness
	var curChannel *Channel
	var curJob *Job

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
				curJob = nil
				section = "harness"
			case "channel":
				cfg.Channel = append(cfg.Channel, Channel{Enabled: true}) // enabled defaults true
				curChannel = &cfg.Channel[len(cfg.Channel)-1]
				curHarness = nil
				curJob = nil
				section = "channel"
			case "job":
				cfg.Job = append(cfg.Job, Job{Enabled: true}) // enabled defaults true
				curJob = &cfg.Job[len(cfg.Job)-1]
				curHarness = nil
				curChannel = nil
				section = "job"
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
			curJob = nil
			continue
		}
		// key = value
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			return nil, fmt.Errorf("config line %d: expected key = value, got %q", lineNo, line)
		}
		key := strings.TrimSpace(line[:eq])
		raw := strings.TrimSpace(line[eq+1:])
		if err := assign(cfg, section, curHarness, curChannel, curJob, key, raw); err != nil {
			return nil, fmt.Errorf("config line %d: %w", lineNo, err)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func assign(cfg *Config, section string, h *Harness, ch *Channel, j *Job, key, raw string) error {
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
	case "session":
		switch key {
		case "context_reset_pressure":
			f, err := asFloat(raw)
			if err != nil {
				return fmt.Errorf("context_reset_pressure: %w", err)
			}
			cfg.Session.ContextResetPressure = f
		case "idle_timeout":
			d, err := asDuration(raw)
			if err != nil {
				return fmt.Errorf("idle_timeout: %w", err)
			}
			cfg.Session.IdleTimeout = d
		case "max_turns":
			n, err := asInt(raw)
			if err != nil {
				return fmt.Errorf("max_turns: %w", err)
			}
			cfg.Session.MaxTurns = int(n)
		case "max_wallclock":
			d, err := asDuration(raw)
			if err != nil {
				return fmt.Errorf("max_wallclock: %w", err)
			}
			cfg.Session.MaxWallclock = d
		case "context_window":
			n, err := asInt(raw)
			if err != nil {
				return fmt.Errorf("context_window: %w", err)
			}
			cfg.Session.ContextWindow = int(n)
		case "gc_interval":
			d, err := asDuration(raw)
			if err != nil {
				return fmt.Errorf("gc_interval: %w", err)
			}
			cfg.Session.GCInterval = d
		case "checkpoint_timeout":
			d, err := asDuration(raw)
			if err != nil {
				return fmt.Errorf("checkpoint_timeout: %w", err)
			}
			cfg.Session.CheckpointTimeout = d
		default:
			return fmt.Errorf("unknown [session] key %q", key)
		}
	case "media":
		switch key {
		case "enabled":
			b, err := asBool(raw)
			if err != nil {
				return fmt.Errorf("enabled: %w", err)
			}
			cfg.Media.Enabled = b
			return nil
		case "max_file_mb":
			n, err := asInt(raw)
			if err != nil {
				return fmt.Errorf("max_file_mb: %w", err)
			}
			cfg.Media.MaxFileMB = int(n)
			return nil
		case "retention":
			d, err := asDuration(raw)
			if err != nil {
				return fmt.Errorf("retention: %w", err)
			}
			cfg.Media.Retention = d
			return nil
		}
		s, err := asString(raw)
		if err != nil {
			return err
		}
		switch key {
		case "whisper_key":
			cfg.Media.WhisperKey = s
		case "whisper_model":
			cfg.Media.WhisperModel = s
		case "whisper_lang":
			cfg.Media.WhisperLang = s
		case "media_dir":
			cfg.Media.Dir = s
		default:
			return fmt.Errorf("unknown [media] key %q", key)
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
		case "enabled":
			b, err := asBool(raw)
			if err != nil {
				return fmt.Errorf("enabled: %w", err)
			}
			ch.Enabled = b
			return nil
		case "allow":
			arr, err := asStringArray(raw)
			if err != nil {
				return err
			}
			ch.Allow = arr
			return nil
		}
		s, err := asString(raw)
		if err != nil {
			return err
		}
		switch key {
		case "kind":
			ch.Kind = s
		case "token":
			ch.Token = s
		case "policy":
			ch.Policy = s
		case "path":
			ch.Path = s
		case "title":
			ch.Title = s
		default:
			return fmt.Errorf("unknown [[channel]] key %q", key)
		}
	case "job":
		switch key {
		case "enabled":
			b, err := asBool(raw)
			if err != nil {
				return fmt.Errorf("enabled: %w", err)
			}
			j.Enabled = b
			return nil
		case "poll":
			d, err := asDuration(raw)
			if err != nil {
				return fmt.Errorf("poll: %w", err)
			}
			j.Poll = d
			return nil
		}
		s, err := asString(raw)
		if err != nil {
			return err
		}
		switch key {
		case "name":
			j.Name = s
		case "trigger":
			j.Trigger = s
		case "schedule":
			j.Schedule = s
		case "tz":
			j.TZ = s
		case "workspace":
			j.Workspace = s
		case "prompt":
			j.Prompt = s
		case "notify":
			j.Notify = s
		case "path":
			j.Path = s
		default:
			return fmt.Errorf("unknown [[job]] key %q", key)
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

// asInt parses a bare integer value (trailing comment tolerated).
func asInt(raw string) (int64, error) {
	return strconv.ParseInt(stripComment(raw), 10, 64)
}

// asBool parses a bare boolean value (trailing comment tolerated).
func asBool(raw string) (bool, error) {
	return strconv.ParseBool(stripComment(raw))
}

// asFloat parses a bare float value (trailing comment tolerated).
func asFloat(raw string) (float64, error) {
	return strconv.ParseFloat(stripComment(raw), 64)
}

// asDuration parses a duration. It accepts a quoted or bare Go duration string
// ("30m", "8h", "120s"); a bare integer is treated as seconds for convenience.
func asDuration(raw string) (time.Duration, error) {
	s := stripComment(raw)
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		s = s[1 : len(s)-1]
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return time.Duration(n) * time.Second, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("expected a duration like \"30m\" or a number of seconds, got %q", s)
	}
	return d, nil
}

package config

import (
	"os"
	"strings"
	"testing"
	"time"
)

const sample = `
# agentd config
[server]
bind       = "127.0.0.1:9099"
api_bearer = "secret-bearer"
state_dir  = "/var/lib/agentd"

[[harness]]
kind  = "claude-code"
model = "claude-sonnet"
cwd   = "/home/joe/work"
skip_permissions = true

[[channel]]
kind   = "telegram"
token  = "env:TG_BOT_TOKEN"      # resolved from environment
allow  = ["123456789", "123"]
policy = "dm-only"
`

func TestParseSample(t *testing.T) {
	cfg, err := Parse([]byte(sample))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Server.Bind != "127.0.0.1:9099" {
		t.Errorf("bind = %q", cfg.Server.Bind)
	}
	if cfg.Server.APIBearer != "secret-bearer" {
		t.Errorf("api_bearer = %q", cfg.Server.APIBearer)
	}
	if cfg.Server.StateDir != "/var/lib/agentd" {
		t.Errorf("state_dir = %q", cfg.Server.StateDir)
	}
	if len(cfg.Harness) != 1 || cfg.Harness[0].Kind != "claude-code" || cfg.Harness[0].Model != "claude-sonnet" {
		t.Fatalf("harness = %+v", cfg.Harness)
	}
	if cfg.Harness[0].Cwd != "/home/joe/work" {
		t.Errorf("harness cwd = %q", cfg.Harness[0].Cwd)
	}
	if !cfg.Harness[0].SkipPermissions {
		t.Errorf("harness skip_permissions = %v, want true", cfg.Harness[0].SkipPermissions)
	}
	if len(cfg.Channel) != 1 {
		t.Fatalf("channels = %d, want 1", len(cfg.Channel))
	}
	c := cfg.Channel[0]
	if c.Kind != "telegram" || c.Token != "env:TG_BOT_TOKEN" || c.Policy != "dm-only" {
		t.Errorf("channel = %+v", c)
	}
	if len(c.Allow) != 2 || c.Allow[0] != "123456789" || c.Allow[1] != "123" {
		t.Errorf("allow = %v", c.Allow)
	}
}

func TestResolveToken(t *testing.T) {
	os.Setenv("TG_BOT_TOKEN", "12345:abc")
	defer os.Unsetenv("TG_BOT_TOKEN")
	if got := ResolveToken("env:TG_BOT_TOKEN"); got != "12345:abc" {
		t.Errorf("ResolveToken env = %q", got)
	}
	if got := ResolveToken("literal-token"); got != "literal-token" {
		t.Errorf("ResolveToken literal = %q", got)
	}
}

func TestParseRejectsUnknownKey(t *testing.T) {
	_, err := Parse([]byte("[server]\nbogus = \"x\"\n"))
	if err == nil {
		t.Fatal("expected error for unknown key")
	}
}

func TestParseMultipleChannelsAndHarnesses(t *testing.T) {
	cfg, err := Parse([]byte(`
[[harness]]
kind = "claude-code"
[[harness]]
kind = "codex"
[[channel]]
kind = "telegram"
[[channel]]
kind = "web"
title = "Joe's agent"
path = "/ui"
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(cfg.Harness) != 2 || cfg.Harness[1].Kind != "codex" {
		t.Fatalf("harness = %+v", cfg.Harness)
	}
	if len(cfg.Channel) != 2 || cfg.Channel[1].Kind != "web" {
		t.Fatalf("channel = %+v", cfg.Channel)
	}
	// enabled defaults true for every channel block.
	if !cfg.Channel[0].Enabled || !cfg.Channel[1].Enabled {
		t.Fatalf("channels should default enabled=true: %+v", cfg.Channel)
	}
	if cfg.Channel[1].Title != "Joe's agent" || cfg.Channel[1].Path != "/ui" {
		t.Fatalf("web channel fields = %+v", cfg.Channel[1])
	}
}

func TestParseChannelEnabledFalse(t *testing.T) {
	cfg, err := Parse([]byte(`
[[channel]]
kind = "web"
enabled = false
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Channel[0].Enabled {
		t.Fatal("enabled=false should park the channel")
	}
}

func TestParseChannelReactions(t *testing.T) {
	cfg, err := Parse([]byte(`
[[channel]]
kind = "telegram"

[[channel]]
kind = "telegram"
reactions = false
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !cfg.Channel[0].Reactions {
		t.Error("reactions should default true")
	}
	if cfg.Channel[1].Reactions {
		t.Error("reactions=false should park emoji progress feedback")
	}
	// Strict key switch: an unknown channel key is still an error.
	if _, err := Parse([]byte("[[channel]]\nkind = \"telegram\"\nreaction = true\n")); err == nil {
		t.Error("expected unknown [[channel]] key to be rejected")
	}
}

func TestParseWhatsAppChannel(t *testing.T) {
	cfg, err := Parse([]byte(`
[[channel]]
kind      = "whatsapp"
bin       = "/Users/me/bin/wacli"
store     = "/Users/me/.wacli"
allow     = [ "41791234567@s.whatsapp.net", "120363000000@g.us" ]
poll      = "15s"
sync      = true
reactions = false
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(cfg.Channel) != 1 {
		t.Fatalf("channels = %d, want 1", len(cfg.Channel))
	}
	c := cfg.Channel[0]
	if c.Kind != "whatsapp" {
		t.Errorf("kind = %q", c.Kind)
	}
	if c.Bin != "/Users/me/bin/wacli" {
		t.Errorf("bin = %q", c.Bin)
	}
	if c.Store != "/Users/me/.wacli" {
		t.Errorf("store = %q", c.Store)
	}
	if c.Poll != 15*time.Second {
		t.Errorf("poll = %v, want 15s", c.Poll)
	}
	if !c.Sync {
		t.Error("sync = false, want true")
	}
	if c.Reactions {
		t.Error("reactions = true, want false")
	}
	if len(c.Allow) != 2 || c.Allow[1] != "120363000000@g.us" {
		t.Errorf("allow = %v", c.Allow)
	}
	// The whatsapp channel carries NO token: auth is the existing wacli store.
	if c.Token != "" {
		t.Errorf("token = %q, want empty", c.Token)
	}
	// Defaults when the keys are absent.
	cfg2, err := Parse([]byte("[[channel]]\nkind = \"whatsapp\"\n"))
	if err != nil {
		t.Fatalf("Parse defaults: %v", err)
	}
	if cfg2.Channel[0].Sync {
		t.Error("sync must default false so it does not fight an external syncer")
	}
	if cfg2.Channel[0].Poll != 0 {
		t.Error("poll must default to zero so the adapter picks its own default")
	}
	if !cfg2.Channel[0].Reactions {
		t.Error("reactions should default true")
	}
	if !cfg2.Channel[0].ReadReceipts {
		t.Error("read_receipts should default true")
	}
}

func TestParseWhatsAppAccessModel(t *testing.T) {
	cfg, err := Parse([]byte(`
[[channel]]
kind            = "whatsapp"
allow           = [ "41791234567@s.whatsapp.net", "188884444777@lid" ]
policy_dm       = "allowlist"
policy_group    = "locked"
allow_groups    = [ "120363000000@g.us" ]
readonly_groups = [ "120363111111@g.us", "120363222222@g.us" ]
read_receipts   = false
stale_after     = "45m"
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	c := cfg.Channel[0]
	if c.PolicyDM != "allowlist" || c.PolicyGroup != "locked" {
		t.Errorf("policies = %q/%q", c.PolicyDM, c.PolicyGroup)
	}
	if len(c.AllowGroups) != 1 || c.AllowGroups[0] != "120363000000@g.us" {
		t.Errorf("allow_groups = %v", c.AllowGroups)
	}
	if len(c.ReadonlyGroups) != 2 {
		t.Errorf("readonly_groups = %v", c.ReadonlyGroups)
	}
	if c.ReadReceipts {
		t.Error("read_receipts = true, want false")
	}
	if c.StaleAfter != 45*time.Minute {
		t.Errorf("stale_after = %v, want 45m", c.StaleAfter)
	}
	// The config example documents BOTH JID forms for one peer, because a real
	// @lid has different digits from the phone JID.
	if len(c.Allow) != 2 {
		t.Fatalf("allow = %v", c.Allow)
	}
	if !strings.HasSuffix(c.Allow[1], "@lid") {
		t.Errorf("second allow entry should be the @lid form, got %q", c.Allow[1])
	}
	// Strict keys still apply to the new surface.
	if _, err := Parse([]byte("[[channel]]\nkind = \"whatsapp\"\npolicy_dms = \"open\"\n")); err == nil {
		t.Error("expected an unknown whatsapp channel key to be rejected")
	}
}

func TestParseWorkspaceTable(t *testing.T) {
	cfg, err := Parse([]byte(`
[workspace]
root    = "/tmp/ws"
default = "joe"
git_autocommit = false
remote  = "git@github.com:me/ws-backup.git"
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	w := cfg.Workspace
	if w.Root != "/tmp/ws" || w.Default != "joe" || w.GitAutocommit || w.Remote != "git@github.com:me/ws-backup.git" {
		t.Fatalf("workspace = %+v", w)
	}
	// Defaults: git_autocommit is ON when the table is absent.
	cfg2, err := Parse([]byte("[server]\nbind = \"127.0.0.1:1\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg2.Workspace.GitAutocommit {
		t.Fatal("git_autocommit should default to true")
	}
}

// TestParseSessionBlock: the [session] block parses the context-reset tunables
// (float, durations, ints) and defaults apply when absent or partial.
func TestParseSessionBlock(t *testing.T) {
	cfg, err := Parse([]byte(`
[session]
context_reset_pressure = 0.6
idle_timeout      = "15m"
max_turns         = 50
max_wallclock     = "4h"
context_window    = 1000000
gc_interval       = "30s"
checkpoint_timeout = "90s"
turn_timeout      = "20m"
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	s := cfg.Session
	if s.ContextResetPressure != 0.6 {
		t.Errorf("context_reset_pressure = %v", s.ContextResetPressure)
	}
	if s.IdleTimeout != 15*time.Minute {
		t.Errorf("idle_timeout = %v", s.IdleTimeout)
	}
	if s.MaxTurns != 50 {
		t.Errorf("max_turns = %d", s.MaxTurns)
	}
	if s.MaxWallclock != 4*time.Hour {
		t.Errorf("max_wallclock = %v", s.MaxWallclock)
	}
	if s.ContextWindow != 1000000 {
		t.Errorf("context_window = %d", s.ContextWindow)
	}
	if s.GCInterval != 30*time.Second {
		t.Errorf("gc_interval = %v", s.GCInterval)
	}
	if s.CheckpointTimeout != 90*time.Second {
		t.Errorf("checkpoint_timeout = %v", s.CheckpointTimeout)
	}
	if s.TurnTimeout != 20*time.Minute {
		t.Errorf("turn_timeout = %v", s.TurnTimeout)
	}

	// Defaults when [session] is absent.
	def, err := Parse([]byte("[server]\nbind = \"127.0.0.1:1\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if def.Session.ContextResetPressure != 0.75 || def.Session.IdleTimeout != 30*time.Minute ||
		def.Session.MaxTurns != 200 || def.Session.ContextWindow != 200000 {
		t.Fatalf("session defaults not applied: %+v", def.Session)
	}
	// A generous default turn budget is load-bearing: 150s silently discarded
	// the answer to any turn that did real research.
	if def.Session.TurnTimeout < 10*time.Minute {
		t.Fatalf("default turn_timeout = %v, want >= 10m", def.Session.TurnTimeout)
	}
}

func TestParseMediaBlockDefaultsAndOverrides(t *testing.T) {
	// No [media] block: full defaults.
	cfg, err := Parse([]byte("[server]\nbind = \"127.0.0.1:1\"\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !cfg.Media.Enabled || cfg.Media.WhisperModel != "whisper-1" ||
		cfg.Media.MaxFileMB != 20 || cfg.Media.Retention != 168*time.Hour ||
		cfg.Media.WhisperLang != "" || cfg.Media.Dir != "" {
		t.Errorf("media defaults wrong: %+v", cfg.Media)
	}

	// Explicit block round-trips every key.
	src := `
[media]
enabled       = true
whisper_key   = "env:OPENAI_API_KEY"
whisper_model = "whisper-1"
whisper_lang  = "de"
max_file_mb   = 15
retention     = "72h"
media_dir     = "/tmp/agentd-media"
`
	cfg, err = Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	m := cfg.Media
	if m.WhisperKey != "env:OPENAI_API_KEY" || m.WhisperLang != "de" ||
		m.MaxFileMB != 15 || m.Retention != 72*time.Hour || m.Dir != "/tmp/agentd-media" {
		t.Errorf("media = %+v", m)
	}

	// whisper_key supports the same env: indirection as channel tokens.
	os.Setenv("AGENTD_TEST_WHISPER", "sk-live-123")
	defer os.Unsetenv("AGENTD_TEST_WHISPER")
	if got := ResolveToken("env:AGENTD_TEST_WHISPER"); got != "sk-live-123" {
		t.Errorf("ResolveToken = %q", got)
	}
	if got := ResolveToken("sk-literal"); got != "sk-literal" {
		t.Errorf("literal ResolveToken = %q", got)
	}

	// Strict key switch: unknown [media] key is rejected.
	if _, err := Parse([]byte("[media]\nbogus = \"x\"\n")); err == nil {
		t.Error("expected error for unknown [media] key")
	}
}

func TestParseMediaEnabledFalse(t *testing.T) {
	cfg, err := Parse([]byte("[media]\nenabled = false\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Media.Enabled {
		t.Error("enabled = true, want false")
	}
}

// TestShippedExampleParses guards against the documentation and the strict-key
// parser drifting apart: config.example.toml is what a user copies, so a key
// documented there that the parser rejects is a broken first run.
func TestShippedExampleParses(t *testing.T) {
	b, err := os.ReadFile("../../config.example.toml")
	if err != nil {
		t.Skipf("example config not readable: %v", err)
	}
	cfg, err := Parse(b)
	if err != nil {
		t.Fatalf("config.example.toml does not parse: %v", err)
	}
	var wa *Channel
	for i := range cfg.Channel {
		if cfg.Channel[i].Kind == "whatsapp" {
			wa = &cfg.Channel[i]
		}
	}
	if wa == nil {
		t.Fatal("the example should document a whatsapp channel")
	}
	// It must ship DISABLED: WhatsApp is a wide-open inbound surface and
	// enabling it is a deliberate act.
	if wa.Enabled {
		t.Error("the whatsapp channel must ship disabled in the example")
	}
	// It must demonstrate BOTH JID forms, or users will hit the @lid trap.
	var sawLid bool
	for _, a := range wa.Allow {
		if strings.HasSuffix(a, "@lid") {
			sawLid = true
		}
	}
	if !sawLid {
		t.Error("the example allow list must show an @lid entry alongside the phone JID")
	}
}

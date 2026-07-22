package config

import (
	"testing"
	"time"
)

// TestParseJobs: [[job]] blocks parse declaratively, with enabled defaulting
// to true and poll parsing as a duration.
func TestParseJobs(t *testing.T) {
	cfg, err := Parse([]byte(`
[server]
bind = "127.0.0.1:1"

[[job]]
name      = "morning-triage"
trigger   = "schedule"
schedule  = "@daily 07:00"
tz        = "Europe/Zurich"
workspace = "default"
prompt    = "Run the morning triage."
notify    = "issues"

[[job]]
name     = "hourly-beat"
schedule = "@every 1h"
prompt   = "Heartbeat."
enabled  = false
notify   = "never"

[[job]]
name    = "inbox-drop"
trigger = "file"
path    = "/tmp/inbox"
poll    = "15s"
prompt  = "Process the drop."

[[job]]
name    = "deploy-hook"
trigger = "webhook"
prompt  = "Verify the deploy."
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(cfg.Job) != 4 {
		t.Fatalf("parsed %d jobs, want 4", len(cfg.Job))
	}
	j := cfg.Job[0]
	if j.Name != "morning-triage" || j.Trigger != "schedule" || j.Schedule != "@daily 07:00" ||
		j.TZ != "Europe/Zurich" || j.Workspace != "default" || j.Notify != "issues" || !j.Enabled {
		t.Fatalf("job[0] = %+v", j)
	}
	if j.Prompt != "Run the morning triage." {
		t.Fatalf("job[0].Prompt = %q", j.Prompt)
	}
	if cfg.Job[1].Enabled {
		t.Fatal("enabled = false not honored")
	}
	if cfg.Job[1].Trigger != "" || cfg.Job[1].Schedule != "@every 1h" {
		t.Fatalf("job[1] = %+v", cfg.Job[1])
	}
	if cfg.Job[2].Path != "/tmp/inbox" || cfg.Job[2].Poll != 15*time.Second {
		t.Fatalf("job[2] = %+v", cfg.Job[2])
	}
	if cfg.Job[3].Trigger != "webhook" {
		t.Fatalf("job[3] = %+v", cfg.Job[3])
	}
}

func TestParseJobUnknownKey(t *testing.T) {
	_, err := Parse([]byte("[[job]]\nname = \"x\"\nbogus = \"y\"\n"))
	if err == nil {
		t.Fatal("unknown [[job]] key accepted")
	}
}

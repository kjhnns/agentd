package main

import (
	"github.com/kjhnns/agentd/internal/config"
	"testing"
)

func TestConfiguredHarness(t *testing.T) {
	for _, kind := range []string{"", "claude-code", "codex", "typo"} {
		cfg := &config.Config{Harness: []config.Harness{{Kind: kind}}}
		a, err := configuredHarness(cfg)
		if kind == "typo" {
			if err == nil {
				t.Fatal("unknown backend accepted")
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		want := kind
		if want == "" {
			want = "claude-code"
		}
		if a.Name() != want {
			t.Fatal(a.Name())
		}
	}
	if _, err := configuredHarness(&config.Config{Harness: make([]config.Harness, 2)}); err == nil {
		t.Fatal("multiple backends silently ignored")
	}
	cfg, err := config.Parse([]byte("[[harness]]\nkind = \"codex\"\nbin = \"/opt/bin/codex\"\nmodel = \"example-model\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Harness[0].Bin != "/opt/bin/codex" {
		t.Fatal(cfg.Harness)
	}
	if _, err = configuredHarness(cfg); err != nil {
		t.Fatal(err)
	}
}

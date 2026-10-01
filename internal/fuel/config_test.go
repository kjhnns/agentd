package fuel

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDaemonConfig(t *testing.T) {
	c, err := ParseDaemonConfig([]byte("# fueld\nlisten = \"127.0.0.1:1\" # here\ntoken = \"env:FUEL_TOKEN\"\nfood_log_var = \"Fuel e2e food log\"\ntest_mode = true\n"))
	if err != nil || c.Listen != "127.0.0.1:1" || !c.TestMode || c.FoodLogVar != "Fuel e2e food log" || c.StateDir != "~/.local/state/fueld" || c.BodyVar != "Body composition" {
		t.Fatalf("%+v %v", c, err)
	}
	if d := DefaultDaemonConfig(); d.Listen != "100.120.65.8:8796" {
		t.Fatal(d.Listen)
	}
	for _, bad := range []string{"bogus = \"x\"\n", "[fuel]\n", "token = SECRET-XYZ\n", "test_mode = SECRET-XYZ\n", "model_provider = \"anthropic\"\n", "token\n"} {
		if _, err := ParseDaemonConfig([]byte(bad)); err == nil || strings.Contains(err.Error(), "SECRET-XYZ") {
			t.Fatalf("%q: %v", bad, err)
		}
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "config.toml")
	_ = os.WriteFile(p, []byte("listen = \"127.0.0.1:1\"\n"), 0o644)
	if _, err := LoadDaemonConfig(p); err == nil {
		t.Fatal("group/world-readable config accepted")
	}
	for _, m := range []os.FileMode{0o400, 0o700, 0o640} {
		_ = os.Chmod(p, m)
		if _, err := LoadDaemonConfig(p); err == nil {
			t.Fatalf("mode %o accepted", m)
		}
	}
	if _, err := LoadDaemonConfig(dir); err == nil {
		t.Fatal("a directory accepted")
	}
	_ = os.Chmod(p, 0o600)
	if _, err := LoadDaemonConfig(p); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FUELD_X", "v")
	if ResolveSecret("env:FUELD_X") != "v" || ResolveSecret("lit") != "lit" {
		t.Fatal("ResolveSecret")
	}
}

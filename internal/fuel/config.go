package fuel

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// DaemonConfig is fueld's own config file (docs/specs/2026-10-fuel-api.md
// section 15). Flat TOML: `key = "string"` or `test_mode = true|false`,
// comments with #. Unknown keys are refused.
type DaemonConfig struct {
	Listen        string
	Token         string // literal or "env:VAR"
	VariablesURL  string
	VariablesKey  string // literal or "env:VAR"
	FoodLogVar    string
	BodyVar       string
	ModelProvider string
	Model         string
	ModelKey      string // literal or "env:VAR"
	ModelEffort   string
	WhisperModel  string
	TargetsFile   string
	StaplesFile   string
	StateDir      string
	StravaDir     string
	TestMode      bool
}

// DefaultConfigPath is where fueld looks without -config.
func DefaultConfigPath() string { return expandHome("~/.config/fueld/config.toml") }

// DefaultDaemonConfig returns the built-in values.
func DefaultDaemonConfig() DaemonConfig {
	return DaemonConfig{
		Listen:        "100.120.65.8:8796",
		VariablesURL:  "https://app.logvariables.com/api/ext",
		FoodLogVar:    "Food log",
		BodyVar:       "Body composition",
		ModelProvider: "openai",
		Model:         "gpt-5.1",
		WhisperModel:  "whisper-1",
		TargetsFile:   "~/.agentd/fuel-targets.json",
		StaplesFile:   "~/.agentd/fuel-staples.json",
		StateDir:      "~/.local/state/fueld",
		StravaDir:     "~/warehouse/raw-sources/strava",
	}
}

// LoadDaemonConfig reads and parses the config file. The file may hold
// literal secrets, so it must not be readable by group or others (0600).
func LoadDaemonConfig(path string) (DaemonConfig, error) {
	f, err := os.Open(expandHome(path))
	if err != nil {
		return DaemonConfig{}, err
	}
	defer f.Close()
	fi, err := f.Stat() // the descriptor we read, not the path again
	if err != nil {
		return DaemonConfig{}, err
	}
	if !fi.Mode().IsRegular() {
		return DaemonConfig{}, fmt.Errorf("%s is not a regular file", filepath.Base(path))
	}
	if fi.Mode().Perm() != 0o600 {
		return DaemonConfig{}, fmt.Errorf("%s is mode %o; it may hold secrets, chmod 600 it", filepath.Base(path), fi.Mode().Perm())
	}
	b, err := io.ReadAll(io.LimitReader(f, 1<<20))
	if err != nil {
		return DaemonConfig{}, err
	}
	return ParseDaemonConfig(b)
}

// ParseDaemonConfig parses config bytes. Errors never echo a value (it may
// be a token with a typo).
func ParseDaemonConfig(b []byte) (DaemonConfig, error) {
	c := DefaultDaemonConfig()
	sc := bufio.NewScanner(strings.NewReader(string(b)))
	n := 0
	for sc.Scan() {
		n++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			return c, fmt.Errorf("line %d: sections are not used in the fueld config", n)
		}
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			return c, fmt.Errorf("line %d: expected key = value", n)
		}
		key := strings.TrimSpace(line[:eq])
		raw := stripTOMLComment(strings.TrimSpace(line[eq+1:]))
		if key == "test_mode" {
			v, err := strconv.ParseBool(raw)
			if err != nil {
				return c, fmt.Errorf("line %d: test_mode expects true or false", n)
			}
			c.TestMode = v
			continue
		}
		if len(raw) < 2 || raw[0] != '"' || raw[len(raw)-1] != '"' {
			return c, fmt.Errorf("line %d: %s expects a quoted string", n, key)
		}
		v := raw[1 : len(raw)-1]
		dst := map[string]*string{
			"listen": &c.Listen, "token": &c.Token, "variables_url": &c.VariablesURL,
			"variables_key": &c.VariablesKey, "food_log_var": &c.FoodLogVar, "body_var": &c.BodyVar,
			"model_provider": &c.ModelProvider, "model": &c.Model, "model_key": &c.ModelKey,
			"model_effort": &c.ModelEffort, "whisper_model": &c.WhisperModel,
			"targets_file": &c.TargetsFile, "staples_file": &c.StaplesFile,
			"state_dir": &c.StateDir, "strava_dir": &c.StravaDir,
		}[key]
		if dst == nil {
			return c, fmt.Errorf("line %d: unknown key %q", n, key)
		}
		*dst = v
	}
	if err := sc.Err(); err != nil {
		return c, err
	}
	if c.Listen == "" {
		return c, errors.New("listen is empty")
	}
	if c.ModelProvider != "openai" {
		return c, fmt.Errorf("model_provider %q is not supported (openai)", c.ModelProvider)
	}
	return c, nil
}

func stripTOMLComment(raw string) string {
	in := false
	for i := 0; i < len(raw); i++ {
		switch raw[i] {
		case '"':
			in = !in
		case '#':
			if !in {
				return strings.TrimSpace(raw[:i])
			}
		}
	}
	return strings.TrimSpace(raw)
}

// ResolveSecret expands "env:VAR"; a literal is returned unchanged.
func ResolveSecret(v string) string {
	if strings.HasPrefix(v, "env:") {
		return os.Getenv(strings.TrimPrefix(v, "env:"))
	}
	return v
}

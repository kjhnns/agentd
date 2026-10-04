package claudecode

import (
	"testing"
	"time"

	"github.com/kjhnns/agentd/internal/harness"
)

// TestExtractUsageFromResult parses token usage from a recorded stream-json
// `result` event and sums the context-occupying tokens (input + cache_read +
// cache_creation + output).
func TestExtractUsageFromResult(t *testing.T) {
	line := []byte(`{"type":"result","subtype":"success","is_error":false,"result":"done",` +
		`"usage":{"input_tokens":100,"cache_creation_input_tokens":5000,"cache_read_input_tokens":144000,"output_tokens":900},` +
		`"session_id":"s"}`)
	total, ok, fromResult := extractUsage(line)
	if !ok || !fromResult {
		t.Fatalf("extractUsage on a result line = (%d,%v,%v), want (_,true,true)", total, ok, fromResult)
	}
	if total != 150000 { // 100 + 5000 + 144000 + 900
		t.Fatalf("total tokens = %d, want 150000", total)
	}
}

// TestResultUsageDoesNotOverridePerRequestUsage: the result line sums every
// request of the turn (here 12 steps against a 70k window = 840k); pressure
// must stay at the last per-request figure, else every multi-step turn reads
// as a full window and the session resets after one turn (the 2026-09-27 bug).
func TestResultUsageDoesNotOverridePerRequestUsage(t *testing.T) {
	h := &handle{id: "s", contextWindow: 200000, proxyBudget: harness.DefaultProxyBudget(), startedAt: time.Now()}
	apply := func(line string) {
		if total, ok, fromResult := extractUsage([]byte(line)); ok {
			if !fromResult || !h.haveUsage {
				h.lastTotalTokens = total
				h.haveUsage = true
			}
		}
	}
	apply(`{"type":"assistant","message":{"usage":{"input_tokens":100,"cache_read_input_tokens":69000,"cache_creation_input_tokens":500,"output_tokens":400},"content":[]},"session_id":"s"}`)
	apply(`{"type":"assistant","message":{"usage":{"input_tokens":100,"cache_read_input_tokens":70000,"cache_creation_input_tokens":600,"output_tokens":300},"content":[]},"session_id":"s"}`)
	apply(`{"type":"result","subtype":"success","usage":{"input_tokens":1200,"cache_read_input_tokens":830000,"cache_creation_input_tokens":6000,"output_tokens":4000},"session_id":"s"}`)
	cp := New("claude").Pressure(h)
	if cp.Tokens != 71000 {
		t.Fatalf("tokens = %d, want 71000 (last per-request usage), result sum must not win", cp.Tokens)
	}
	if cp.Fraction > 0.36 {
		t.Fatalf("fraction = %.2f, want ~0.355", cp.Fraction)
	}
	// A turn with no assistant usage at all still gets the result fallback.
	h2 := &handle{id: "s2", contextWindow: 200000, proxyBudget: harness.DefaultProxyBudget(), startedAt: time.Now()}
	if total, ok, fromResult := extractUsage([]byte(`{"type":"result","usage":{"input_tokens":50,"output_tokens":50},"session_id":"s2"}`)); ok && (!fromResult || !h2.haveUsage) {
		h2.lastTotalTokens = total
		h2.haveUsage = true
	}
	if !h2.haveUsage || h2.lastTotalTokens != 100 {
		t.Fatalf("result fallback not applied: have=%v tokens=%d", h2.haveUsage, h2.lastTotalTokens)
	}
}

// TestExtractUsageFromAssistant parses usage nested under an assistant message.
func TestExtractUsageFromAssistant(t *testing.T) {
	line := []byte(`{"type":"assistant","message":{"usage":{"input_tokens":10,"cache_read_input_tokens":20,"output_tokens":30},"content":[{"type":"text","text":"hi"}]},"session_id":"s"}`)
	total, ok, fromResult := extractUsage(line)
	if !ok || total != 60 || fromResult {
		t.Fatalf("assistant usage = (%d,%v,%v), want (60,true,false)", total, ok, fromResult)
	}
}

// TestExtractUsageNone: a line with no usage yields (0,false), so pressure stays
// on the proxy backstop.
func TestExtractUsageNone(t *testing.T) {
	if total, ok, _ := extractUsage([]byte(`{"type":"system","subtype":"init","session_id":"s"}`)); ok || total != 0 {
		t.Fatalf("no-usage line = (%d,%v), want (0,false)", total, ok)
	}
}

// TestRealPressureFraction: after a usage line is observed, Pressure is REAL and
// equals measured tokens / context window.
func TestRealPressureFraction(t *testing.T) {
	line := []byte(`{"type":"result","subtype":"success","usage":{"input_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":150000,"output_tokens":0},"session_id":"s"}`)
	total, ok, _ := extractUsage(line)
	if !ok {
		t.Fatal("no usage parsed")
	}
	h := &handle{
		id:              "s",
		contextWindow:   200000,
		lastTotalTokens: total,
		haveUsage:       true,
		proxyBudget:     harness.DefaultProxyBudget(),
		startedAt:       time.Now(),
	}
	a := New("claude")
	cp := a.Pressure(h)
	if cp.Source != harness.PressureReal {
		t.Fatalf("source = %q, want real", cp.Source)
	}
	if cp.Tokens != 150000 || cp.Window != 200000 {
		t.Fatalf("tokens/window = %d/%d, want 150000/200000", cp.Tokens, cp.Window)
	}
	if cp.Fraction < 0.749 || cp.Fraction > 0.751 {
		t.Fatalf("fraction = %v, want ~0.75", cp.Fraction)
	}
}

// TestProxyPressureBeforeUsage: with no usage seen yet, Pressure falls back to a
// proxy estimate (source=proxy), so an early session still has a signal.
func TestProxyPressureBeforeUsage(t *testing.T) {
	h := &handle{
		id:            "s",
		contextWindow: 200000,
		proxyBudget:   harness.ProxyBudget{Turns: 10, Bytes: 1 << 30, Wallclock: time.Hour},
		startedAt:     time.Now(),
		turns:         5,
	}
	a := New("claude")
	cp := a.Pressure(h)
	if cp.Source != harness.PressureProxy {
		t.Fatalf("source = %q, want proxy", cp.Source)
	}
	if cp.Fraction < 0.49 || cp.Fraction > 0.51 { // 5/10 turns
		t.Fatalf("proxy fraction = %v, want ~0.5", cp.Fraction)
	}
}

// TestResolveWindow: a known model id wins over the adapter default.
func TestResolveWindow(t *testing.T) {
	a := New("claude").WithContextWindow(123456)
	if got := a.resolveWindow(""); got != 123456 {
		t.Fatalf("unknown model window = %d, want configured 123456", got)
	}
	if got := a.resolveWindow("claude-opus-4"); got != 200000 {
		t.Fatalf("known model window = %d, want 200000", got)
	}
}

// TestPermissionModeArgs: PermissionMode overrides the SkipPermissions bool.
func TestPermissionModeArgs(t *testing.T) {
	has := func(args []string, flag string) bool {
		for _, a := range args {
			if a == flag {
				return true
			}
		}
		return false
	}
	// prompt mode strips the skip flag even when SkipPermissions=true.
	args := buildArgs(harness.SessionConfig{SessionID: "s", SkipPermissions: true, PermissionMode: harness.PermissionPrompt}, "")
	if has(args, "--dangerously-skip-permissions") {
		t.Fatalf("prompt mode must not pass --dangerously-skip-permissions: %v", args)
	}
	// skip mode adds it even when SkipPermissions=false.
	args = buildArgs(harness.SessionConfig{SessionID: "s", SkipPermissions: false, PermissionMode: harness.PermissionSkip}, "")
	if !has(args, "--dangerously-skip-permissions") {
		t.Fatalf("skip mode must pass --dangerously-skip-permissions: %v", args)
	}
}

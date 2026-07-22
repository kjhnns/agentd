// Package harness defines the HarnessAdapter interface (design section 3.2), the
// agnosticism boundary. The core drives every backend (Claude Code, Codex, any
// CLI agent) only through this interface, so a second adapter is a drop-in.
package harness

import (
	"context"

	"github.com/kjhnns/agentd/internal/eventbus"
)

// Capabilities lets the core pick the richest available mode per adapter.
type Capabilities struct {
	StructuredEvents    bool `json:"structured_events"`     // native stream-json / SDK events?
	Interrupt           bool `json:"interrupt"`             // can it be interrupted mid-turn?
	Resume              bool `json:"resume"`                // can a prior session resume by id?
	RealContextPressure bool `json:"real_context_pressure"` // reports MEASURED token usage (else proxy-only)
}

// PermissionMode is the tool-approval policy for a session, modeled as a value
// (not a hard-coded flag) so a future ACP-based adapter can map it to ACP's
// client-side permission handling instead of a CLI flag. Empty means "fall back
// to SessionConfig.SkipPermissions" for backward compatibility.
type PermissionMode string

const (
	PermissionSkip   PermissionMode = "skip"   // run tools without asking (hands-free)
	PermissionPrompt PermissionMode = "prompt" // keep the harness's own approval prompts
)

// Status is the coarse lifecycle state of a running session.
type Status string

const (
	StatusIdle          Status = "idle"
	StatusThinking      Status = "thinking"
	StatusAwaitingInput Status = "awaiting_input"
	StatusError         Status = "error"
	StatusDead          Status = "dead"
)

// SessionConfig configures a session on start.
type SessionConfig struct {
	SessionID       string            // agentd's session id (stable across restarts)
	Cwd             string            // working directory for the agent
	Model           string            // model override (optional)
	SystemPrompt    string            // injected server-owned context (design 3.5)
	SkipPermissions bool              // run the harness with its permission guardrail bypassed
	PermissionMode  PermissionMode    // ACP-ready permission policy; overrides SkipPermissions when set
	Extra           map[string]string // adapter-specific knobs
}

// Input is a user turn or a control key. Exactly one of Text/Key is set.
type Input struct {
	Text string // a free-text user turn
	Key  string // a control key: "enter", "ctrl-c", "y", "n", ...
}

// Handle is an opaque per-session handle owned by an adapter.
type Handle interface {
	// ID returns the agentd session id this handle belongs to.
	ID() string
}

// Adapter is the HarnessAdapter contract (design 3.2). Implementations must be
// safe for concurrent use across distinct handles.
type Adapter interface {
	// identity / capability
	Name() string
	Capabilities() Capabilities

	// lifecycle
	Start(ctx context.Context, cfg SessionConfig) (Handle, error)
	Attach(ctx context.Context, existing string) (Handle, error)
	Send(ctx context.Context, h Handle, in Input) error
	Interrupt(h Handle) error
	Status(h Handle) Status
	Teardown(h Handle) error

	// Pressure reports the live session's normalized context pressure (0..1)
	// plus its source (real|proxy). The core reads this after each completed
	// turn to decide whether to perform a controlled context reset. An adapter
	// with no usage instrumentation returns a proxy estimate.
	Pressure(h Handle) ContextPressure

	// output: normalized event stream (design 3.4)
	Events(h Handle) <-chan eventbus.Event
}

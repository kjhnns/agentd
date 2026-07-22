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
	StructuredEvents bool `json:"structured_events"` // native stream-json / SDK events?
	Interrupt        bool `json:"interrupt"`         // can it be interrupted mid-turn?
	Resume           bool `json:"resume"`            // can a prior session resume by id?
}

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

	// output: normalized event stream (design 3.4)
	Events(h Handle) <-chan eventbus.Event
}

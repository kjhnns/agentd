// Package channel defines the ChannelAdapter interface (design section 3.3),
// fully decoupled from the harness. The server owns routing, allowlists, the
// confirm-gate, and message formatting; a channel is a config entry, not custom
// code. A channel is a task INSIDE the one server process (no SSE hop, no MCP
// proxy child, no enabledPlugins flag), which is what structurally deletes the
// five failure classes in design section 2.
package channel

import (
	"context"

	"github.com/kjhnns/agentd/internal/media"
)

// InboundMsg is a normalized inbound message from any channel. Media carries
// artifact references produced by the core media service (media.Ingest); the
// SESSION layer renders them into the canonical turn text
// (session.RenderInbound). Adapters do acquisition only and MUST NOT pre-bake
// marker text into Text.
type InboundMsg struct {
	Channel string           `json:"channel"`  // "telegram", ...
	UserID  string           `json:"user_id"`  // chat id / user id (allowlist key)
	MsgID   string           `json:"msg_id"`   // provider message id; the Ack/react target
	Sender  string           `json:"sender"`   // provider sender id (may differ from UserID in groups)
	TS      int64            `json:"ts"`       // provider send time, unix seconds (0 = unknown)
	Text    string           `json:"text"`     // message text / caption (UNTRUSTED input)
	Media   []media.Artifact `json:"media"`    // ingested artifacts; may be empty
	ReplyTo string           `json:"reply_to"` // id of the message being replied to
}

// Lifecycle reaction emoji. These are the visible progress feedback a user gets
// on their own message while a turn runs, ported behaviour-for-behaviour from
// clawd's tg-bridge chain (daemon 👀 on receipt, UserPromptSubmit hook ⚡, Stop
// hook 👍, stall watcher 😱). Every glyph here is on Telegram's fixed
// bot-reaction whitelist; a channel that cannot react implements Ack as a no-op.
const (
	ReactionReceived   = "👀" // message reached the server
	ReactionWorking    = "⚡" // a session picked it up, turn in flight
	ReactionDone       = "👍" // turn finished, reply sent
	ReactionError      = "😱" // turn failed / timed out
	ReactionNeedsInput = "🤔" // the agent is waiting on the user
)

// OutboundMsg is a normalized outbound message.
type OutboundMsg struct {
	ChatID string   `json:"chat_id"`
	Text   string   `json:"text"`
	Media  []string `json:"media"`
	// Silent asks the channel to deliver without alerting the user. It is set
	// on the long half of a split reply so the wrist buzzes ONCE, for the
	// summary that actually fits a notification. A channel that cannot do this
	// simply ignores it (delivery is never skipped).
	Silent bool `json:"silent,omitempty"`
	// Summary marks the short half of a split reply, so a channel that keeps a
	// conversation can label it ("summary of the message above") instead of
	// letting it read as a complete, self-contained answer.
	Summary bool `json:"summary,omitempty"`
}

// SendReceipt carries a real provider id, the acceptance artifact for a send.
type SendReceipt struct {
	ID string `json:"id"`
}

// Adapter is the ChannelAdapter contract (design 3.3).
type Adapter interface {
	Name() string
	// Start connects (long-poll / webhook / ws) from config only.
	Start(ctx context.Context) error
	// Inbound streams normalized inbound messages (post-allowlist).
	Inbound() <-chan InboundMsg
	// Send delivers an outbound message and returns a real id (evidence).
	Send(ctx context.Context, m OutboundMsg) (SendReceipt, error)
	SupportsMedia() bool
	// Ack is optional read/typing/react feedback on an INBOUND message; a no-op
	// is a valid implementation. It must never block or fail the caller's turn:
	// implementations are best-effort and log their own failures.
	Ack(chatID, msgID, reaction string) error
}

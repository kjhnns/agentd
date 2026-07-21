// Package channel defines the ChannelAdapter interface (design section 3.3),
// fully decoupled from the harness. The server owns routing, allowlists, the
// confirm-gate, and message formatting; a channel is a config entry, not custom
// code. A channel is a task INSIDE the one server process (no SSE hop, no MCP
// proxy child, no enabledPlugins flag), which is what structurally deletes the
// five failure classes in design section 2.
package channel

import "context"

// InboundMsg is a normalized inbound message from any channel.
type InboundMsg struct {
	Channel string   `json:"channel"`  // "telegram", ...
	UserID  string   `json:"user_id"`  // chat id / user id (allowlist key)
	Text    string   `json:"text"`     // message text (UNTRUSTED input)
	Media   []string `json:"media"`    // media refs (paths/urls); may be empty
	ReplyTo string   `json:"reply_to"` // id of the message being replied to
}

// OutboundMsg is a normalized outbound message.
type OutboundMsg struct {
	ChatID string   `json:"chat_id"`
	Text   string   `json:"text"`
	Media  []string `json:"media"`
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
	// Ack is optional read/typing/react; a no-op is a valid implementation.
	Ack(msgID, reaction string) error
}

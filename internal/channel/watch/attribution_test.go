package watch

import (
	"context"
	"testing"

	"github.com/kjhnns/agentd/internal/channel"
)

// TestReplyIsAttributedToItsOwnMessage is the D2 regression: Send ignored
// which message a reply belonged to and attributed it to the OLDEST user
// message still in flight. With two turns in flight (or one queued behind
// another), the second answer landed under the first question.
func TestReplyIsAttributedToItsOwnMessage(t *testing.T) {
	a, st := newAdapter(t)
	first := st.Append(Message{Role: RoleUser, Kind: KindText, Text: "first", Status: StatusWorking})
	second := st.Append(Message{Role: RoleUser, Kind: KindText, Text: "second", Status: StatusWorking})

	rc, err := a.Send(context.Background(), channel.OutboundMsg{Text: "answer to second", InReplyTo: second.ID})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := st.Get(rc.ID)
	if got.ReplyTo != second.ID {
		t.Fatalf("reply attributed to %q, want %q (the first message is %q)", got.ReplyTo, second.ID, first.ID)
	}

	// The failure notice of the first message, sent after its status flipped
	// to failed, is a failure under the first message even though the second
	// is still working.
	_ = a.Ack("", first.ID, channel.ReactionError)
	rc, _ = a.Send(context.Background(), channel.OutboundMsg{Text: "could not", InReplyTo: first.ID})
	got, _ = st.Get(rc.ID)
	if got.ReplyTo != first.ID || got.Kind != KindFailure {
		t.Fatalf("notice = %+v, want a failure under %q", got, first.ID)
	}
}

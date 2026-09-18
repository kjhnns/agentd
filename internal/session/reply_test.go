package session

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kjhnns/agentd/internal/channel"
	"github.com/kjhnns/agentd/internal/eventbus"
)

func TestReplyFormatInstructionIsOffAtZero(t *testing.T) {
	if got := ReplyFormatInstruction(0); got != "" {
		t.Fatalf("budget 0 must add nothing, got %q", got)
	}
	instr := ReplyFormatInstruction(200)
	for _, want := range []string{"200", SummaryMarker, "Apple Watch"} {
		if !strings.Contains(instr, want) {
			t.Errorf("instruction is missing %q: %s", want, instr)
		}
	}
}

func TestRenderTurnAppendsTheInstructionToRealTurnTextOnly(t *testing.T) {
	m := &Manager{Reply: ReplyPolicy{SummaryBudget: 120}}
	in := channel.InboundMsg{Channel: "watch", Text: "what is on my calendar"}
	got := m.RenderTurn(in)
	if !strings.HasPrefix(got, "what is on my calendar\n\n[Reply format:") {
		t.Fatalf("rendered turn = %q", got)
	}
	if !strings.Contains(got, "120") {
		t.Error("the configured budget must reach the agent")
	}
	// Off by config: the turn is the user's text, untouched.
	off := &Manager{Reply: ReplyPolicy{SummaryBudget: 0}}
	if got := off.RenderTurn(in); got != "what is on my calendar" {
		t.Fatalf("budget 0 changed the turn: %q", got)
	}
	// An empty turn stays empty rather than becoming a bare instruction.
	if got := m.RenderTurn(channel.InboundMsg{Channel: "watch"}); got != "" {
		t.Fatalf("empty turn = %q", got)
	}
}

func TestSplitReplyWithoutMarkerIsUntouched(t *testing.T) {
	short := "Two meetings today."
	body, summary := SplitReply(short, 200)
	if body != short || summary != "" {
		t.Fatalf("body=%q summary=%q", body, summary)
	}
	// Even a LONG reply without a marker is delivered whole: inventing a
	// summary is not this layer's job.
	long := strings.Repeat("x", 900)
	body, summary = SplitReply(long, 200)
	if body != long || summary != "" {
		t.Fatalf("long reply was altered: %d chars, summary %q", len(body), summary)
	}
}

func TestSplitReplyTakesTheHalvesApart(t *testing.T) {
	reply := "Here is the long answer.\nIt has several lines.\n\n" + SummaryMarker + "\nTwo meetings: dentist 14:00, Cami 19:30."
	body, summary := SplitReply(reply, 200)
	if body != "Here is the long answer.\nIt has several lines." {
		t.Fatalf("body = %q", body)
	}
	if summary != "Two meetings: dentist 14:00, Cami 19:30." {
		t.Fatalf("summary = %q", summary)
	}
	if strings.Contains(body, SummaryMarker) || strings.Contains(summary, SummaryMarker) {
		t.Error("the marker must not survive into either message")
	}
}

func TestSplitReplyUsesTheLastMarkerAndIgnoresAHalfOne(t *testing.T) {
	// The body legitimately quotes the marker (e.g. explaining this feature).
	reply := "I use " + SummaryMarker + " to split replies.\n" + SummaryMarker + "\nSplitting is done with a marker line."
	body, summary := SplitReply(reply, 200)
	if !strings.Contains(body, "I use") || summary != "Splitting is done with a marker line." {
		t.Fatalf("body=%q summary=%q", body, summary)
	}
	// A marker with nothing after it is not a split: deliver the reply whole
	// rather than sending an empty message.
	whole := "All of the answer.\n" + SummaryMarker + "\n   \n"
	body, summary = SplitReply(whole, 200)
	if body != whole || summary != "" {
		t.Fatalf("trailing marker: body=%q summary=%q", body, summary)
	}
	// Nothing BEFORE the marker is not a split either.
	lead := SummaryMarker + "\njust a summary"
	body, summary = SplitReply(lead, 200)
	if body != lead || summary != "" {
		t.Fatalf("leading marker: body=%q summary=%q", body, summary)
	}
}

func TestSummaryIsCutToTheBudgetOnAWordBoundary(t *testing.T) {
	long := "The dentist moved to Thursday at ten, Cami lands at seven in the evening, and the train tickets are booked for both of you already."
	reply := "full answer\n" + SummaryMarker + "\n" + long
	_, summary := SplitReply(reply, 60)
	if len([]rune(summary)) > 60 {
		t.Fatalf("summary is %d chars, over budget: %q", len([]rune(summary)), summary)
	}
	if !strings.HasSuffix(summary, "…") {
		t.Errorf("a cut summary must say so: %q", summary)
	}
	if strings.HasSuffix(strings.TrimSuffix(summary, "…"), " ") {
		t.Errorf("trailing space before the ellipsis: %q", summary)
	}
	// Inside the budget it is delivered exactly as written.
	reply = "full answer\n" + SummaryMarker + "\nShort enough."
	_, summary = SplitReply(reply, 60)
	if summary != "Short enough." {
		t.Fatalf("summary = %q", summary)
	}
}

func TestSummaryOrWholePicksTheLastMessage(t *testing.T) {
	if got := summaryOrWhole("body", "summary"); got != "summary" {
		t.Errorf("split reply must end with the summary, got %q", got)
	}
	if got := summaryOrWhole("body", ""); got != "body" {
		t.Errorf("unsplit reply must be delivered whole, got %q", got)
	}
}

// recordingChannel captures what the user would actually receive.
type recordingChannel struct {
	mu    sync.Mutex
	sent  []channel.OutboundMsg
	acks  []string
	fails bool
}

func (c *recordingChannel) Name() string                       { return "watch" }
func (c *recordingChannel) Start(context.Context) error        { return nil }
func (c *recordingChannel) Inbound() <-chan channel.InboundMsg { return nil }
func (c *recordingChannel) SupportsMedia() bool                { return false }
func (c *recordingChannel) Ack(chatID, msgID, reaction string) error {
	c.mu.Lock()
	c.acks = append(c.acks, reaction)
	c.mu.Unlock()
	return nil
}
func (c *recordingChannel) Send(ctx context.Context, m channel.OutboundMsg) (channel.SendReceipt, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fails {
		return channel.SendReceipt{}, errors.New("send failed")
	}
	c.sent = append(c.sent, m)
	return channel.SendReceipt{ID: strconv.Itoa(len(c.sent))}, nil
}
func (c *recordingChannel) messages() []channel.OutboundMsg {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]channel.OutboundMsg, len(c.sent))
	copy(out, c.sent)
	return out
}

// routeWithReply drives one full inbound turn whose harness answers `reply`,
// and returns what the channel delivered plus what the harness was asked.
func routeWithReply(t *testing.T, budget int, reply string) ([]channel.OutboundMsg, []string) {
	t.Helper()
	bus := eventbus.New()
	fa := &fakeAdapter{}
	mgr := NewManager(fa, bus, nil)
	mgr.Policy = Policy{TurnTimeout: 5 * time.Second, TurnCeiling: 10 * time.Second}
	mgr.Reply = ReplyPolicy{SummaryBudget: budget}

	sess, err := mgr.Create(context.Background(), "", t.TempDir(), "", "reply-test")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	fa.mu.Lock()
	fa.onSend = func(string) {
		bus.Publish(eventbus.Event{SessionID: sess.ID, Kind: eventbus.KindResult, Text: reply})
	}
	fa.mu.Unlock()

	ch := &recordingChannel{}
	in := channel.InboundMsg{Channel: "watch", UserID: "chat1", MsgID: "m1", Text: "how is the trip looking"}
	if err := mgr.RouteInbound(context.Background(), ch, in, map[string]string{"chat1": sess.ID}, t.TempDir(), ""); err != nil {
		t.Fatalf("RouteInbound: %v", err)
	}
	return ch.messages(), fa.sentTexts()
}

// The behaviour Joe asked for: a long answer arrives as the full text FIRST
// and silently, then the summary, so the summary is the message at the bottom
// of the watch chat and the only one that raises a notification.
func TestRouteInboundDeliversLongAnswerThenSummary(t *testing.T) {
	long := strings.Repeat("Detail about the trip. ", 40)
	sent, asked := routeWithReply(t, 200, long+"\n"+SummaryMarker+"\nFlights are booked, hotel is not.")

	if len(sent) != 2 {
		t.Fatalf("want 2 messages (body then summary), got %d: %+v", len(sent), sent)
	}
	if !strings.HasPrefix(sent[0].Text, "Detail about the trip.") || !sent[0].Silent {
		t.Errorf("first message must be the full answer, delivered silently: silent=%v %.40q", sent[0].Silent, sent[0].Text)
	}
	if sent[1].Text != "Flights are booked, hotel is not." {
		t.Errorf("last message must be the summary, got %q", sent[1].Text)
	}
	if sent[1].Silent {
		t.Error("the summary is the one that must alert the user")
	}
	if strings.Contains(sent[0].Text, SummaryMarker) || strings.Contains(sent[1].Text, SummaryMarker) {
		t.Error("the marker leaked into a delivered message")
	}
	// And the agent was actually told the format, with the configured budget.
	if len(asked) == 0 || !strings.Contains(asked[0], SummaryMarker) || !strings.Contains(asked[0], "200") {
		t.Errorf("the turn did not carry the reply-format instruction: %q", asked)
	}
}

func TestRouteInboundSendsAShortAnswerOnceAndAudibly(t *testing.T) {
	sent, _ := routeWithReply(t, 200, "Two meetings today.")
	if len(sent) != 1 {
		t.Fatalf("a short answer must be ONE message, got %d: %+v", len(sent), sent)
	}
	if sent[0].Text != "Two meetings today." || sent[0].Silent {
		t.Errorf("message = %+v", sent[0])
	}
}

func TestRouteInboundWithTheFeatureOffIsUnchanged(t *testing.T) {
	reply := "Long answer.\n" + SummaryMarker + "\nShort."
	sent, asked := routeWithReply(t, 0, reply)
	if len(sent) != 1 || sent[0].Text != reply {
		t.Fatalf("budget 0 must deliver the reply verbatim, got %+v", sent)
	}
	if strings.Contains(asked[0], "Reply format") {
		t.Error("budget 0 must not instruct the agent about summaries")
	}
}

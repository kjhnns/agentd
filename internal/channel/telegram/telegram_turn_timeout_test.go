package telegram

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kjhnns/agentd/internal/channel"
	"github.com/kjhnns/agentd/internal/eventbus"
	"github.com/kjhnns/agentd/internal/harness"
	"github.com/kjhnns/agentd/internal/media"
	"github.com/kjhnns/agentd/internal/session"
)

// These tests cover the failure Joe actually hit on 2026-07-26: a voice message
// was downloaded, transcribed and routed correctly, the agent answered 170s
// later, and agentd threw the answer away because RouteInbound had a HARDCODED
// 150s bound. He got the 😱 glyph and total silence.
//
// The pre-existing telegram fixture tests all used an instant echo harness, so
// no test ever observed how long a turn is allowed to take or what a user sees
// when it overruns. That was the blind spot, not the media pipeline (which the
// run-log shows transcribed all six real voice notes with zero errors). These
// tests close it by driving the REAL adapter, the REAL media.Ingest and the REAL
// session.RouteInbound against a slow harness.

// slowAdapter is the echo harness with a turn that takes real wall-clock time,
// which is the one property the old fixtures could not express.
type slowAdapter struct {
	echoAdapter
	delay time.Duration
}

func (s slowAdapter) Send(ctx context.Context, h harness.Handle, in harness.Input) error {
	select {
	case <-time.After(s.delay):
	case <-ctx.Done():
		return ctx.Err()
	}
	return s.echoAdapter.Send(ctx, h, in)
}

var _ harness.Adapter = slowAdapter{}

// routeWithPolicy drives one already-normalized inbound through RouteInbound
// under an explicit Policy and returns the resulting error.
func routeWithPolicy(t *testing.T, a *Adapter, ha harness.Adapter, p session.Policy, in channel.InboundMsg) error {
	t.Helper()
	bus := eventbus.New()
	mgr := session.NewManager(ha, bus, nil)
	mgr.Policy = p
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	done := make(chan error, 1)
	go func() { done <- mgr.RouteInbound(ctx, a, in, map[string]string{}, "", "") }()
	select {
	case err := <-done:
		return err
	case <-time.After(20 * time.Second):
		t.Fatal("RouteInbound did not complete")
		return nil
	}
}

// TestTurnBudgetIsConfigurable: the bound on a turn comes from Policy, not from
// a constant baked into RouteInbound. Under the old hardcoded 150s this failed,
// because a 400ms turn with a 100ms budget was delivered instead of abandoned.
func TestTurnBudgetIsConfigurable(t *testing.T) {
	b := newBotStub(t)
	a := New("tok", []string{"111"}).WithReactions(false)
	a.base = b.srv.URL

	in := channel.InboundMsg{Channel: "telegram", UserID: "111", MsgID: "6543", Text: "hi"}
	err := routeWithPolicy(t, a, slowAdapter{delay: 400 * time.Millisecond},
		session.Policy{TurnTimeout: 100 * time.Millisecond}, in)
	if err == nil {
		t.Fatal("turn slower than the configured budget was not abandoned; the budget is being ignored")
	}
	if !strings.Contains(err.Error(), "timed out after 100ms") {
		t.Fatalf("error = %v, want it to name the CONFIGURED budget", err)
	}
}

// TestTurnWithinBudgetDelivers: the same slow turn under a budget that fits is
// delivered normally. This is the direct analogue of Joe's 170s research turn.
func TestTurnWithinBudgetDelivers(t *testing.T) {
	b := newBotStub(t)
	a := New("tok", []string{"111"})
	a.base = b.srv.URL

	in := channel.InboundMsg{Channel: "telegram", UserID: "111", MsgID: "6543", Text: "hi"}
	if err := routeWithPolicy(t, a, slowAdapter{delay: 400 * time.Millisecond},
		session.Policy{TurnTimeout: 10 * time.Second}, in); err != nil {
		t.Fatalf("RouteInbound: %v", err)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.sends != 1 {
		t.Fatalf("sendMessage calls = %d, want 1 (the slow answer must still be delivered)", b.sends)
	}
}

// TestDefaultTurnBudgetSurvivesResearchTurn pins the default. Joe's turn needed
// 170s; anything in the low hundreds of seconds re-introduces the bug for every
// deployment that does not set turn_timeout explicitly.
func TestDefaultTurnBudgetSurvivesResearchTurn(t *testing.T) {
	got := session.DefaultPolicy().TurnTimeout
	if got < 10*time.Minute {
		t.Fatalf("default turn budget = %s, want >= 10m (a web-research turn routinely exceeds 150s)", got)
	}
}

// TestTimedOutVoiceMessageGetsSpokenReason is the end-to-end UX regression: a
// real voice update goes through getFile, download, media.Ingest and
// transcription, the turn then overruns, and the user must be TOLD. Under the
// old code the only feedback was the 😱 reaction and no message at all.
func TestTimedOutVoiceMessageGetsSpokenReason(t *testing.T) {
	bs := newBotServer(t, []byte("OPUSDATA"))
	a, _ := newMediaAdapter(t, bs, &stubTranscriber{
		out: media.Transcript{Text: "what is the fire doing", Language: "english", DurationS: 7}})

	// Real acquisition + ingest + transcription, exactly as handleUpdate does.
	a.handleUpdate(tgUpdate{UpdateID: 9, Message: voiceMsg(111, 7, 100, "")})

	var in channel.InboundMsg
	select {
	case in = <-a.Inbound():
	case <-time.After(3 * time.Second):
		t.Fatal("no inbound emitted for voice update")
	}
	if len(in.Media) != 1 || in.Media[0].Kind != media.KindAudio {
		t.Fatalf("expected one transcribed audio artifact, got %+v", in.Media)
	}

	err := routeWithPolicy(t, a, slowAdapter{delay: 500 * time.Millisecond},
		session.Policy{TurnTimeout: 100 * time.Millisecond}, in)
	if err == nil {
		t.Fatal("expected the overrunning turn to fail")
	}

	sent := bs.waitSent(t, 1)
	notice := sent[len(sent)-1]
	if !strings.Contains(notice, "voice message") {
		t.Errorf("failure notice = %q, want it to name the voice message", notice)
	}
	// The reason wording changed with the taxonomy: slowAdapter emits no
	// progress events at all, so this is the WENT-SILENT branch, not a "ran too
	// long" branch. Naming silence is the whole point: a turn that is visibly
	// working is no longer killed for taking time.
	if !strings.Contains(strings.ToLower(notice), "silent") {
		t.Errorf("failure notice = %q, want it to state the reason (it went silent)", notice)
	}
	if strings.Contains(notice, "*") || strings.Contains(notice, "—") {
		t.Errorf("failure notice = %q, want plain text with no markdown and no em-dash", notice)
	}
}

// TestTimedOutTurnStillReactsError keeps the reaction chain honest: the notice
// is additional feedback, it does not replace the 😱 glyph.
func TestTimedOutTurnStillReactsError(t *testing.T) {
	b := newBotStub(t)
	a := New("tok", []string{"111"})
	a.base = b.srv.URL

	in := channel.InboundMsg{Channel: "telegram", UserID: "111", MsgID: "6543", Text: "hi"}
	if err := routeWithPolicy(t, a, slowAdapter{delay: 500 * time.Millisecond},
		session.Policy{TurnTimeout: 100 * time.Millisecond}, in); err == nil {
		t.Fatal("expected the overrunning turn to fail")
	}

	want := []string{channel.ReactionWorking, channel.ReactionError}
	got := b.waitEmojis(t, want)
	if !sameStrings(got, want) {
		t.Fatalf("reaction chain = %v, want %v", got, want)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.sends != 1 {
		t.Fatalf("sendMessage calls = %d, want 1 (the spoken failure notice)", b.sends)
	}
}

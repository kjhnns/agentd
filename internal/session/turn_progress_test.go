package session

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kjhnns/agentd/internal/channel"
	"github.com/kjhnns/agentd/internal/eventbus"
	"github.com/kjhnns/agentd/internal/harness"
	"github.com/kjhnns/agentd/internal/media"
)

// These tests use a COMPRESSED CLOCK, not a real 15m wait: the quiet budget is
// injected in milliseconds and the turn is measured in multiples of it. A test
// that actually waited out the production 15m bound would never be run, and the
// property under test is the ratio (turn duration vs bound), not the constant.
const testQuiet = 200 * time.Millisecond

// emitEvery emits kind events on out every period until stop closes, then emits
// a final result. It models a sub-agent doing real work: continuous progress,
// total duration far past the quiet bound.
func emitEvery(id string, out chan<- eventbus.Event, period time.Duration, until time.Duration, final string) {
	deadline := time.After(until)
	tick := time.NewTicker(period)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			out <- eventbus.Event{SessionID: id, Kind: eventbus.KindToolCall, Tool: "Bash"}
		case <-deadline:
			if final != "" {
				out <- eventbus.Event{SessionID: id, Kind: eventbus.KindResult, Text: final}
			}
			return
		}
	}
}

// TestLongProgressingTurnSurvivesPastTheOldHardCap is the regression for the
// bug: on 2026-08-10T10:40:28Z a turn was abandoned at exactly 900.1s having
// emitted 45 progress events (38 tool_call, 7 output), its longest silence being
// 138.6s. The harness went on to finish at 10:50:34Z and the answer was thrown
// away. Under the old HARD cap this test fails; under the inactivity bound the
// turn survives 10x its own bound because it never stops making progress.
func TestLongProgressingTurnSurvivesPastTheOldHardCap(t *testing.T) {
	const turnLen = 10 * testQuiet // 10x the bound: the old cap would kill this
	a := &scriptedAdapter{emit: func(id string, out chan<- eventbus.Event) {
		go emitEvery(id, out, testQuiet/4, turnLen, "the answer that used to be discarded")
	}, block: nil}
	// Send must not return before the work is done, exactly like the real
	// adapter blocking on the turn's result event.
	done := make(chan struct{})
	a.block = done
	go func() { time.Sleep(turnLen + 50*time.Millisecond); close(done) }()

	mgr, id := newScripted(t, a)

	start := time.Now()
	got, err := mgr.SendAndCollect(context.Background(), id, "book the hotels one by one",
		TurnOptions{Timeout: testQuiet, Ceiling: 30 * time.Second})
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("a turn that made progress the whole time was abandoned: %v (ran %s, bound %s)", err, elapsed, testQuiet)
	}
	if got != "the answer that used to be discarded" {
		t.Fatalf("result = %q, want the delivered answer", got)
	}
	if elapsed < turnLen {
		t.Fatalf("turn returned after %s, want at least %s: the test did not actually outrun the bound", elapsed, turnLen)
	}
	t.Logf("turn ran %s under a %s bound (%.1fx) and was DELIVERED", elapsed, testQuiet, float64(elapsed)/float64(testQuiet))
}

// TestHungTurnIsStillReaped is the other half of the contract: an inactivity
// bound must not make a wedged turn unkillable. Nothing is ever emitted, so the
// quiet timer runs to expiry and the turn dies at roughly the bound.
func TestHungTurnIsStillReaped(t *testing.T) {
	a := &scriptedAdapter{block: make(chan struct{})} // blocks forever, emits nothing
	mgr, id := newScripted(t, a)

	start := time.Now()
	_, err := mgr.SendAndCollect(context.Background(), id, "wedge",
		TurnOptions{Timeout: testQuiet, Ceiling: 30 * time.Second})
	elapsed := time.Since(start)

	if !errors.Is(err, ErrTurnTimeout) {
		t.Fatalf("err = %v, want ErrTurnTimeout: a hung turn must still be reaped", err)
	}
	if errors.Is(err, ErrTurnCeiling) {
		t.Fatalf("err = %v, want the QUIET branch, not the ceiling backstop", err)
	}
	if elapsed > 5*testQuiet {
		t.Fatalf("hung turn took %s to be reaped, want about %s", elapsed, testQuiet)
	}
	t.Logf("hung turn reaped after %s (bound %s): %v", elapsed, testQuiet, err)
}

// TestStatusHeartbeatDoesNotKeepAHungTurnAlive: only real work resets the timer.
// A status/needs_input stream is not evidence of progress, and if it reset the
// timer a wedged turn that still ticks its status would live forever.
func TestStatusHeartbeatDoesNotKeepAHungTurnAlive(t *testing.T) {
	stop, gone := make(chan struct{}), make(chan struct{})
	a := &scriptedAdapter{emit: func(id string, out chan<- eventbus.Event) {
		go func() {
			defer close(gone)
			tick := time.NewTicker(testQuiet / 5)
			defer tick.Stop()
			for {
				select {
				case <-tick.C:
					out <- eventbus.Event{SessionID: id, Kind: eventbus.KindStatus, Status: "thinking"}
				case <-stop:
					return
				}
			}
		}()
	}, block: make(chan struct{})}
	mgr, id := newScripted(t, a)
	// Registered AFTER newScripted so it runs BEFORE its Teardown (cleanups are
	// LIFO): the emitter must be off the handle's channel before Teardown
	// closes it, or the test itself races.
	t.Cleanup(func() { close(stop); <-gone })

	_, err := mgr.SendAndCollect(context.Background(), id, "wedge",
		TurnOptions{Timeout: testQuiet, Ceiling: 30 * time.Second})
	if !errors.Is(err, ErrTurnTimeout) {
		t.Fatalf("err = %v, want ErrTurnTimeout: a status heartbeat must not count as progress", err)
	}
}

// TestCeilingReapsARunawayThatKeepsEmittingProgress proves the backstop. This is
// the pathological case an inactivity timer alone cannot catch: a loop that
// emits a tool call forever would reset the quiet timer forever.
func TestCeilingReapsARunawayThatKeepsEmittingProgress(t *testing.T) {
	stop, gone := make(chan struct{}), make(chan struct{})
	a := &scriptedAdapter{emit: func(id string, out chan<- eventbus.Event) {
		go func() {
			defer close(gone)
			tick := time.NewTicker(10 * time.Millisecond)
			defer tick.Stop()
			for {
				select {
				case <-tick.C:
					out <- eventbus.Event{SessionID: id, Kind: eventbus.KindToolCall, Tool: "Bash"}
				case <-stop:
					return
				}
			}
		}()
	}, block: make(chan struct{})}
	mgr, id := newScripted(t, a)
	// See the note in TestStatusHeartbeat...: stop the emitter BEFORE Teardown
	// closes the channel it writes to.
	t.Cleanup(func() { close(stop); <-gone })

	ceiling := 400 * time.Millisecond
	start := time.Now()
	// Quiet bound deliberately huge so it can never be what fires.
	_, err := mgr.SendAndCollect(context.Background(), id, "runaway",
		TurnOptions{Timeout: 30 * time.Second, Ceiling: ceiling})
	elapsed := time.Since(start)

	if !errors.Is(err, ErrTurnCeiling) {
		t.Fatalf("err = %v, want ErrTurnCeiling: a runaway that keeps emitting progress must still be reaped", err)
	}
	// A ceiling error must ALSO read as a timeout, so the API's 504 mapping and
	// every existing errors.Is(ErrTurnTimeout) caller keep working.
	if !errors.Is(err, ErrTurnTimeout) {
		t.Fatalf("err = %v, want it to also satisfy errors.Is(ErrTurnTimeout)", err)
	}
	if elapsed > 4*ceiling {
		t.Fatalf("runaway took %s to be reaped, want about %s", elapsed, ceiling)
	}
	var te *TurnTimeoutError
	if !errors.As(err, &te) || te.Steps == 0 {
		t.Fatalf("err = %v, want a TurnTimeoutError recording the steps it made", err)
	}
	t.Logf("runaway reaped by the ceiling after %s with %d steps: %v", elapsed, te.Steps, err)
}

// TestTimeoutPreservesPartialWork: an abandoned turn has usually produced real
// output. Returning "" and a bare error throws it away.
func TestTimeoutPreservesPartialWork(t *testing.T) {
	a := &scriptedAdapter{emit: func(id string, out chan<- eventbus.Event) {
		out <- eventbus.Event{SessionID: id, Kind: eventbus.KindOutput, Text: "Booked the Shanghai hotel, refundable."}
	}, block: make(chan struct{})}
	mgr, id := newScripted(t, a)

	partial, err := mgr.SendAndCollect(context.Background(), id, "book them",
		TurnOptions{Timeout: testQuiet, Ceiling: 30 * time.Second})
	if !errors.Is(err, ErrTurnTimeout) {
		t.Fatalf("err = %v, want ErrTurnTimeout", err)
	}
	if !strings.Contains(partial, "Shanghai") {
		t.Fatalf("partial = %q, want the work done before the turn was abandoned", partial)
	}
	notice := failureNotice(channel.InboundMsg{}, err)
	if !strings.Contains(notice, "Shanghai") {
		t.Fatalf("failure notice = %q, want it to carry the partial work", notice)
	}
}

// TestDefaultQuietBoundClearsTheObservedWorstSilence pins the production
// constant against MEASURED data rather than taste. The longest silence inside a
// real working turn in the whole run log is 138.6s (2026-08-10 10:38:09.388934Z
// to 10:40:28.002041Z, session bdf2128e29f7b794). A default at or under that
// re-introduces the bug for every deployment that does not tune it.
func TestDefaultQuietBoundClearsTheObservedWorstSilence(t *testing.T) {
	const observedWorstSilence = 139 * time.Second
	got := DefaultPolicy().TurnTimeout
	if got < 4*observedWorstSilence {
		t.Fatalf("default quiet bound = %s, want >= 4x the observed worst in-turn silence (%s)", got, observedWorstSilence)
	}
	// And the ceiling has to sit above any real turn (longest observed: 25m)
	// but below the handle-age reset, or it would pre-empt max_wallclock.
	c := DefaultPolicy().TurnCeiling
	if c <= 25*time.Minute {
		t.Fatalf("default ceiling = %s, want above the longest real turn observed (25m)", c)
	}
	if c >= DefaultPolicy().MaxWallclock {
		t.Fatalf("default ceiling = %s, want below max_wallclock %s", c, DefaultPolicy().MaxWallclock)
	}
}

// TestFailureNoticeDistinguishesEveryCause is the UX contract. Joe could not
// tell a dead Claude login from a timeout from a restart, because they all
// rendered as "Sorry, I could not process that message: <raw go error>". Each
// cause must produce its own actionable sentence, and the branches must be
// mutually exclusive: in particular an AUTH failure must never be reported as a
// timeout, whatever else it wraps.
func TestFailureNoticeDistinguishesEveryCause(t *testing.T) {
	in := channel.InboundMsg{}
	cases := []struct {
		name    string
		err     error
		want    []string // all must appear
		notWant []string // none may appear
	}{
		{
			name: "auth",
			// The shape the claudecode adapter actually returns: its own prose
			// plus the shared sentinel, classified once by isAuthError.
			err: fmt.Errorf("claudecode: turn error: Failed to authenticate: OAuth session expired and could not be refreshed: %w", harness.ErrAuth),
			// what died, what to do, and that it was not processed
			want:    []string{"Claude login has expired", "/login", "send it again"},
			notWant: []string{"time budget", "silent", "hard limit", "OAuth session expired and could not be refreshed"},
		},
		{
			name:    "cancel",
			err:     fmt.Errorf("route: %w", context.Canceled),
			want:    []string{"stopped while working"},
			notWant: []string{"login", "silent", "hard limit"},
		},
		{
			name:    "quiet timeout",
			err:     &TurnTimeoutError{SessionID: "s", Elapsed: 15 * time.Minute, Quiet: 15 * time.Minute, Budget: 15 * time.Minute},
			want:    []string{"silent"},
			notWant: []string{"login", "hard limit", "process died"},
		},
		{
			name:    "ceiling",
			err:     &TurnTimeoutError{SessionID: "s", Ceiling: true, Elapsed: 2 * time.Hour, Budget: 2 * time.Hour, Steps: 400},
			want:    []string{"hard limit", "still running"},
			notWant: []string{"login", "silent", "process died"},
		},
		{
			name:    "process gone",
			err:     fmt.Errorf("claudecode: session s process exited before result: %w", harness.ErrProcessGone),
			want:    []string{"process died"},
			notWant: []string{"login", "silent", "hard limit"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := failureNotice(in, tc.err)
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("notice = %q, want it to contain %q", got, w)
				}
			}
			for _, n := range tc.notWant {
				if strings.Contains(got, n) {
					t.Errorf("notice = %q, must NOT contain %q (causes are being confused)", got, n)
				}
			}
			// House style, enforced everywhere Joe reads: plain text on a phone.
			if strings.Contains(got, "—") || strings.Contains(got, "*") {
				t.Errorf("notice = %q, want plain text with no em-dash and no markdown", got)
			}
			t.Logf("%s -> %s", tc.name, got)
		})
	}
}

// TestAuthNoticeWinsOverATimeoutWrap is the mutual-exclusion guard with teeth:
// even an error that satisfies BOTH classifiers must be reported as the auth
// failure, because that is the only one the user can act on.
func TestAuthNoticeWinsOverATimeoutWrap(t *testing.T) {
	err := fmt.Errorf("%w: %w", ErrTurnTimeout, harness.ErrAuth)
	got := failureNotice(channel.InboundMsg{}, err)
	if !strings.Contains(got, "Claude login has expired") {
		t.Fatalf("notice = %q, want the auth branch to win", got)
	}
}

// TestFailureNoticeNamesTheInput keeps the pre-existing property: a failed voice
// note must read as a failed voice note in EVERY new branch, not just the old
// timeout one.
func TestFailureNoticeNamesTheInput(t *testing.T) {
	voice := channel.InboundMsg{Media: []media.Artifact{{Kind: media.KindAudio}}}
	for _, err := range []error{
		fmt.Errorf("x: %w", harness.ErrAuth),
		fmt.Errorf("x: %w", context.Canceled),
		&TurnTimeoutError{Budget: time.Minute},
		&TurnTimeoutError{Budget: time.Minute, Ceiling: true},
		fmt.Errorf("x: %w", harness.ErrProcessGone),
	} {
		if got := failureNotice(voice, err); !strings.Contains(got, "voice message") {
			t.Errorf("notice for %v = %q, want it to name the voice message", err, got)
		}
	}
}

package scheduler

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kjhnns/agentd/internal/eventbus"
	"github.com/kjhnns/agentd/internal/harness"
	"github.com/kjhnns/agentd/internal/session"
	"github.com/kjhnns/agentd/internal/workspace"
)

// fakeRunner is an in-memory Runner: no live harness anywhere in these tests.
type fakeRunner struct {
	mu     sync.Mutex
	calls  []string // titles, in order
	failFn func(title string) error
	result string
}

func (r *fakeRunner) RunTurn(ctx context.Context, ws, prompt, title string) (string, error) {
	r.mu.Lock()
	r.calls = append(r.calls, title)
	fail := r.failFn
	res := r.result
	r.mu.Unlock()
	if fail != nil {
		if err := fail(title); err != nil {
			return "", err
		}
	}
	return res, nil
}

func (r *fakeRunner) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

func waitFor(t *testing.T, d time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for: %s", msg)
}

func everyJob(t *testing.T, name, spec string) *Job {
	t.Helper()
	sched, err := ParseSchedule(spec, time.UTC)
	if err != nil {
		t.Fatalf("ParseSchedule: %v", err)
	}
	return &Job{Name: name, Enabled: true, Prompt: "do the thing",
		Trigger: Trigger{Kind: TriggerSchedule, Spec: spec, Schedule: sched}}
}

// TestScheduleFiresProducesJobRun: a "@every" job fires on schedule (real fast
// interval, fake runner, no live claude) and produces an ok JobRun in the
// history plus a durable job_fire/job_run record in the state file.
func TestScheduleFiresProducesJobRun(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "sched.jsonl")
	fr := &fakeRunner{result: "heartbeat appended"}
	job := everyJob(t, "beat", "@every 100ms")
	s, err := New([]*Job{job}, fr, statePath, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()
	s.Tick = 20 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)

	waitFor(t, 3*time.Second, func() bool {
		runs, _ := s.Runs("beat", 5)
		for _, r := range runs {
			if r.Status == "ok" && r.Trigger == "schedule" {
				return true
			}
		}
		return false
	}, "an ok schedule-triggered JobRun")

	runs, _ := s.Runs("beat", 1)
	if runs[0].Result != "heartbeat appended" {
		t.Fatalf("run result = %q", runs[0].Result)
	}
	if fr.callCount() == 0 {
		t.Fatal("runner never called")
	}
	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("state file: %v", err)
	}
	for _, want := range []string{`"job_fire"`, `"job_run"`, `"sched_time"`} {
		if !strings.Contains(string(data), want) {
			t.Errorf("state file missing %s:\n%s", want, data)
		}
	}
}

// TestDurabilityRestartNoDoubleFire: a fired schedule occurrence is persisted;
// a NEW scheduler over the same state file reloads it and does NOT re-fire the
// same occurrence, only the next one. Run history also survives the restart.
func TestDurabilityRestartNoDoubleFire(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "sched.jsonl")
	fr := &fakeRunner{}
	mkJob := func() *Job {
		sched, _ := ParseSchedule("@daily 07:00", time.UTC)
		return &Job{Name: "triage", Enabled: true, Prompt: "triage",
			Trigger: Trigger{Kind: TriggerSchedule, Spec: "@daily 07:00", Schedule: sched}}
	}

	s1, err := New([]*Job{mkJob()}, fr, statePath, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Yesterday's occurrence already fired; today's 07:00 comes due at 07:00:30.
	s1.lastFire["triage"] = time.Date(2026, 7, 21, 7, 0, 0, 0, time.UTC)
	s1.checkDue(time.Date(2026, 7, 22, 7, 0, 30, 0, time.UTC))
	if len(s1.runq) != 1 {
		t.Fatalf("first checkDue queued %d runs, want 1", len(s1.runq))
	}
	<-s1.runq // drain (no worker running)
	// Record a finished run so history durability is also exercised.
	s1.recordRunLocked(mkJob(), JobRun{JobID: "triage", RunID: "r1", Trigger: "schedule",
		Start: time.Now().UTC(), End: time.Now().UTC(), Status: "ok", Result: "clean"})
	if err := s1.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// RESTART: fresh scheduler over the same state file.
	s2, err := New([]*Job{mkJob()}, fr, statePath, nil)
	if err != nil {
		t.Fatalf("New (restart): %v", err)
	}
	defer s2.Close()
	if got, want := s2.lastFire["triage"], time.Date(2026, 7, 22, 7, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("restored lastFire = %v, want %v", got, want)
	}
	// Same morning, minutes later: the already-fired 07:00 must NOT re-fire.
	s2.checkDue(time.Date(2026, 7, 22, 7, 5, 0, 0, time.UTC))
	if len(s2.runq) != 0 {
		t.Fatalf("restart double-fired the already-run schedule (%d queued)", len(s2.runq))
	}
	// The NEXT day's occurrence still fires.
	s2.checkDue(time.Date(2026, 7, 23, 7, 0, 10, 0, time.UTC))
	if len(s2.runq) != 1 {
		t.Fatalf("next-day occurrence did not fire (%d queued)", len(s2.runq))
	}
	// History replayed.
	runs, err := s2.Runs("triage", 10)
	if err != nil || len(runs) == 0 || runs[0].RunID != "r1" {
		t.Fatalf("run history not replayed after restart: %v %v", runs, err)
	}
}

// TestMissedOccurrencesSkippedWithLog: the documented catch-up policy. Fires
// missed during downtime (older than CatchupGrace) are SKIPPED with a durable
// job_skip record, and the skip itself survives a restart (no re-skip loop, no
// late fire).
func TestMissedOccurrencesSkippedWithLog(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "sched.jsonl")
	fr := &fakeRunner{}
	sched, _ := ParseSchedule("@daily 07:00", time.UTC)
	job := &Job{Name: "triage", Enabled: true, Prompt: "triage",
		Trigger: Trigger{Kind: TriggerSchedule, Spec: "@daily 07:00", Schedule: sched}}

	s, err := New([]*Job{job}, fr, statePath, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Last fired 3 days ago; server was down over the 07:00s since. It is now
	// 09:00, i.e. 2h past the latest occurrence: beyond the 5m grace -> skip.
	s.lastFire["triage"] = time.Date(2026, 7, 19, 7, 0, 0, 0, time.UTC)
	s.checkDue(time.Date(2026, 7, 22, 9, 0, 0, 0, time.UTC))
	if len(s.runq) != 0 {
		t.Fatalf("stale occurrences fired (%d queued); catch-up policy is skip", len(s.runq))
	}
	if got, want := s.lastFire["triage"], time.Date(2026, 7, 22, 7, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("schedule position not advanced past skipped occurrences: %v", got)
	}
	data, _ := os.ReadFile(statePath)
	if !strings.Contains(string(data), `"job_skip"`) {
		t.Fatalf("no job_skip record logged:\n%s", data)
	}
	s.Close()

	// Restart: the skip record must have advanced the durable position too.
	s2, err := New([]*Job{job}, fr, statePath, nil)
	if err != nil {
		t.Fatalf("New (restart): %v", err)
	}
	defer s2.Close()
	s2.checkDue(time.Date(2026, 7, 22, 9, 1, 0, 0, time.UTC))
	if len(s2.runq) != 0 {
		t.Fatal("restart re-fired/skip-looped an already-skipped occurrence")
	}
}

// TestNotifyPolicyIssuesOnly: with the default issues-only policy a CLEAN run
// is recorded but NOT notified; a FAILED run is notified (hook + job_notify
// record). An "always" job notifies on clean runs too.
func TestNotifyPolicyIssuesOnly(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "sched.jsonl")
	fr := &fakeRunner{result: "all clean", failFn: func(title string) error {
		if strings.Contains(title, "broken") {
			return errors.New("boom")
		}
		return nil
	}}
	clean := &Job{Name: "clean", Enabled: true, Prompt: "p", Notify: NotifyIssues, Trigger: Trigger{Kind: TriggerWebhook}}
	broken := &Job{Name: "broken", Enabled: true, Prompt: "p", Notify: NotifyIssues, Trigger: Trigger{Kind: TriggerWebhook}}
	chatty := &Job{Name: "chatty", Enabled: true, Prompt: "p", Notify: NotifyAlways, Trigger: Trigger{Kind: TriggerWebhook}}

	s, err := New([]*Job{clean, broken, chatty}, fr, statePath, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()
	var nmu sync.Mutex
	notified := map[string]int{}
	s.OnNotify = func(j *Job, run JobRun) {
		nmu.Lock()
		notified[j.Name]++
		nmu.Unlock()
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)

	for _, name := range []string{"clean", "broken", "chatty"} {
		if q, reason, err := s.RunNow(name, "manual"); err != nil || !q {
			t.Fatalf("RunNow(%s): queued=%v reason=%q err=%v", name, q, reason, err)
		}
	}
	waitFor(t, 3*time.Second, func() bool {
		done := 0
		for _, name := range []string{"clean", "broken", "chatty"} {
			if runs, _ := s.Runs(name, 1); len(runs) > 0 && runs[0].Status != "" {
				done++
			}
		}
		return done == 3
	}, "all three runs recorded")

	cleanRuns, _ := s.Runs("clean", 1)
	if cleanRuns[0].Status != "ok" || cleanRuns[0].Notified {
		t.Fatalf("clean run: status=%s notified=%v; want ok + suppressed", cleanRuns[0].Status, cleanRuns[0].Notified)
	}
	brokenRuns, _ := s.Runs("broken", 1)
	if brokenRuns[0].Status != "error" || !brokenRuns[0].Notified {
		t.Fatalf("broken run: status=%s notified=%v; want error + notified", brokenRuns[0].Status, brokenRuns[0].Notified)
	}
	chattyRuns, _ := s.Runs("chatty", 1)
	if chattyRuns[0].Status != "ok" || !chattyRuns[0].Notified {
		t.Fatalf("chatty run: status=%s notified=%v; want ok + notified (always)", chattyRuns[0].Status, chattyRuns[0].Notified)
	}
	waitFor(t, time.Second, func() bool {
		nmu.Lock()
		defer nmu.Unlock()
		return notified["broken"] == 1 && notified["chatty"] == 1
	}, "notify hook for broken + chatty")
	nmu.Lock()
	if notified["clean"] != 0 {
		t.Fatalf("clean run reached the notify hook %d times; issues-only must suppress it", notified["clean"])
	}
	nmu.Unlock()
	data, _ := os.ReadFile(statePath)
	if !strings.Contains(string(data), `"job_notify"`) {
		t.Fatal("no job_notify record in state log")
	}
}

// TestOverlapSkip: firing a job whose previous run is still active records a
// skipped run instead of stacking a second concurrent run.
func TestOverlapSkip(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "sched.jsonl")
	release := make(chan struct{})
	fr := &fakeRunner{failFn: func(title string) error {
		<-release
		return nil
	}}
	job := &Job{Name: "slow", Enabled: true, Prompt: "p", Trigger: Trigger{Kind: TriggerWebhook}}
	s, err := New([]*Job{job}, fr, statePath, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)

	if q, _, err := s.RunNow("slow", "manual"); err != nil || !q {
		t.Fatalf("first RunNow: %v", err)
	}
	waitFor(t, 2*time.Second, func() bool { return fr.callCount() == 1 }, "first run started")

	q, reason, err := s.RunNow("slow", "manual")
	if err != nil {
		t.Fatalf("second RunNow: %v", err)
	}
	if q {
		t.Fatal("second fire queued while first still running; want overlap skip")
	}
	if !strings.Contains(reason, "still active") {
		t.Fatalf("skip reason = %q", reason)
	}
	runs, _ := s.Runs("slow", 5)
	if runs[0].Status != "skipped" {
		t.Fatalf("newest run status = %s, want skipped", runs[0].Status)
	}
	close(release)
	waitFor(t, 2*time.Second, func() bool {
		runs, _ := s.Runs("slow", 5)
		for _, r := range runs {
			if r.Status == "ok" {
				return true
			}
		}
		return false
	}, "first run finished ok")
}

// TestFileWatchTriggerFires: a file-trigger job fires when content under the
// watched path changes (poll-based signature watcher).
func TestFileWatchTriggerFires(t *testing.T) {
	watched := t.TempDir()
	statePath := filepath.Join(t.TempDir(), "sched.jsonl")
	fr := &fakeRunner{result: "handled the drop"}
	job := &Job{Name: "inbox", Enabled: true, Prompt: "process new files", Notify: NotifyNever,
		Trigger: Trigger{Kind: TriggerFile, Spec: watched, Path: watched, Poll: 20 * time.Millisecond}}
	s, err := New([]*Job{job}, fr, statePath, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)

	time.Sleep(60 * time.Millisecond) // let the baseline scan land
	if fr.callCount() != 0 {
		t.Fatal("watcher fired on pre-existing state (baseline must not fire)")
	}
	if err := os.WriteFile(filepath.Join(watched, "drop.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, func() bool {
		runs, _ := s.Runs("inbox", 3)
		for _, r := range runs {
			if r.Trigger == "file" && r.Status == "ok" {
				return true
			}
		}
		return false
	}, "a file-triggered ok run")
}

// ---- serialization against a REAL session.Manager ----

// blockingAdapter is a harness fake whose Send blocks until released, so the
// test can hold an "in-progress interactive turn" and prove a job fire queues
// behind it (session.turnMu) instead of running concurrently.
type blockingAdapter struct {
	mu      sync.Mutex
	sends   []string
	release chan struct{} // every Send waits for one token
}

type blockHandle struct {
	id     string
	events chan eventbus.Event
}

func (h *blockHandle) ID() string { return h.id }

func (a *blockingAdapter) Name() string                       { return "blocking-fake" }
func (a *blockingAdapter) Capabilities() harness.Capabilities { return harness.Capabilities{} }
func (a *blockingAdapter) Interrupt(h harness.Handle) error   { return nil }
func (a *blockingAdapter) Status(h harness.Handle) harness.Status {
	return harness.StatusIdle
}
func (a *blockingAdapter) Pressure(h harness.Handle) harness.ContextPressure {
	return harness.ContextPressure{}
}
func (a *blockingAdapter) Start(ctx context.Context, cfg harness.SessionConfig) (harness.Handle, error) {
	return &blockHandle{id: cfg.SessionID, events: make(chan eventbus.Event, 8)}, nil
}
func (a *blockingAdapter) Attach(ctx context.Context, existing string) (harness.Handle, error) {
	return &blockHandle{id: existing, events: make(chan eventbus.Event, 8)}, nil
}
func (a *blockingAdapter) Teardown(h harness.Handle) error {
	if bh, ok := h.(*blockHandle); ok {
		close(bh.events)
	}
	return nil
}
func (a *blockingAdapter) Events(h harness.Handle) <-chan eventbus.Event {
	return h.(*blockHandle).events
}
func (a *blockingAdapter) Send(ctx context.Context, h harness.Handle, in harness.Input) error {
	a.mu.Lock()
	a.sends = append(a.sends, in.Text)
	a.mu.Unlock()
	<-a.release
	// Emit the turn's result so ManagerRunner gets a summary.
	if bh, ok := h.(*blockHandle); ok {
		bh.events <- eventbus.Event{SessionID: bh.id, Kind: eventbus.KindResult, Text: "done"}
	}
	return nil
}
func (a *blockingAdapter) sendCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.sends)
}
func (a *blockingAdapter) sentTexts() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, len(a.sends))
	copy(out, a.sends)
	return out
}

// TestJobSerializesBehindInteractiveTurn: a job fired while an interactive
// turn is running in the SAME workspace QUEUES (session turn lock) and only
// runs after the interactive turn completes. This exercises the real
// session.Manager + ManagerRunner path with a fake harness.
func TestJobSerializesBehindInteractiveTurn(t *testing.T) {
	ba := &blockingAdapter{release: make(chan struct{})}
	bus := eventbus.New()
	mgr := session.NewManager(ba, bus, nil)
	mgr.Policy = session.Policy{} // no lifecycle triggers in this test
	mgr.Workspaces = workspace.NewStore(t.TempDir(), "default")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sess, err := mgr.Create(ctx, "default", "", "", "interactive")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Hold an interactive turn open.
	interactiveDone := make(chan error, 1)
	go func() {
		_, err := mgr.Send(ctx, sess.ID, "INTERACTIVE: long running user turn")
		interactiveDone <- err
	}()
	waitFor(t, 2*time.Second, func() bool { return ba.sendCount() == 1 }, "interactive turn started")

	// Fire a job at the SAME workspace while the interactive turn runs.
	statePath := filepath.Join(t.TempDir(), "sched.jsonl")
	job := &Job{Name: "triage", Enabled: true, Workspace: "default",
		Prompt: "JOB: run the triage checklist", Notify: NotifyNever,
		Trigger: Trigger{Kind: TriggerWebhook}}
	runner := &ManagerRunner{Mgr: mgr, Bus: bus}
	s, err := New([]*Job{job}, runner, statePath, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()
	s.Start(ctx)
	if q, _, err := s.RunNow("triage", "manual"); err != nil || !q {
		t.Fatalf("RunNow: %v", err)
	}

	// The job must NOT start a harness turn while the interactive one runs.
	time.Sleep(150 * time.Millisecond)
	if got := ba.sendCount(); got != 1 {
		t.Fatalf("job turn started concurrently with interactive turn (%d sends)", got)
	}

	// Finish the interactive turn; the queued job turn now runs.
	ba.release <- struct{}{}
	if err := <-interactiveDone; err != nil {
		t.Fatalf("interactive turn: %v", err)
	}
	waitFor(t, 2*time.Second, func() bool { return ba.sendCount() == 2 }, "queued job turn started after interactive finished")
	ba.release <- struct{}{}

	waitFor(t, 3*time.Second, func() bool {
		runs, _ := s.Runs("triage", 1)
		return len(runs) > 0 && runs[0].Status == "ok"
	}, "job run recorded ok")

	sends := ba.sentTexts()
	if !strings.Contains(sends[0], "INTERACTIVE") || !strings.Contains(sends[1], "JOB:") {
		t.Fatalf("turn order wrong: %v", sends)
	}
	// The job REUSED the interactive session (single-active-session per
	// workspace), it did not spawn a parallel one.
	if n := len(mgr.List()); n != 1 {
		t.Fatalf("job spawned a parallel session (%d sessions)", n)
	}
}

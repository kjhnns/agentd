package notify_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kjhnns/agentd/internal/notify"
	"github.com/kjhnns/agentd/internal/scheduler"
)

// fakeSink is a notify-capable channel double: it records every dispatched
// notification on a buffered channel so a test can assert delivery.
type fakeSink struct {
	name string
	got  chan notify.Notification
}

func newFakeSink(name string) *fakeSink {
	return &fakeSink{name: name, got: make(chan notify.Notification, 16)}
}
func (s *fakeSink) Name() string { return s.name }
func (s *fakeSink) Notify(n notify.Notification) error {
	s.got <- n
	return nil
}

func recv(t *testing.T, ch chan notify.Notification, d time.Duration) (notify.Notification, bool) {
	t.Helper()
	select {
	case n := <-ch:
		return n, true
	case <-time.After(d):
		return notify.Notification{}, false
	}
}

// TestDispatchFansOutToAllSinks: a dispatched notification reaches EVERY
// registered sink (the core of the hub: channels register, dispatch fans out).
func TestDispatchFansOutToAllSinks(t *testing.T) {
	hub := notify.NewHub()
	a := newFakeSink("web")
	b := newFakeSink("telegram")
	hub.Register(a)
	hub.Register(b)
	if hub.Count() != 2 {
		t.Fatalf("Count = %d, want 2", hub.Count())
	}

	hub.Dispatch(notify.Notification{Source: "job:x", Level: notify.LevelIssue, Text: "boom"})

	for name, ch := range map[string]chan notify.Notification{"web": a.got, "telegram": b.got} {
		n, ok := recv(t, ch, time.Second)
		if !ok {
			t.Fatalf("sink %s never received the dispatched notification", name)
		}
		if n.Text != "boom" || n.Level != notify.LevelIssue || n.Source != "job:x" {
			t.Fatalf("sink %s got wrong notification: %+v", name, n)
		}
		if n.Ts.IsZero() {
			t.Fatalf("sink %s: Ts not stamped", name)
		}
	}
}

// TestDispatchIsolatesSinkErrors: one sink failing does not stop delivery to the
// others (best-effort fan-out).
func TestDispatchIsolatesSinkErrors(t *testing.T) {
	hub := notify.NewHub()
	bad := &errSink{name: "bad"}
	good := newFakeSink("good")
	hub.Register(bad)
	hub.Register(good)
	hub.Dispatch(notify.Notification{Source: "s", Level: notify.LevelInfo, Text: "hi"})
	if _, ok := recv(t, good.got, time.Second); !ok {
		t.Fatal("a failing sink blocked delivery to a healthy sink")
	}
}

type errSink struct{ name string }

func (s *errSink) Name() string                     { return s.name }
func (s *errSink) Notify(notify.Notification) error { return errors.New("nope") }

// ---- scheduler notify policy -> Notifier -> a (fake web) channel ----

type fakeRunner struct {
	result string
	failOn string // fail runs whose title contains this substring
}

func (r *fakeRunner) RunTurn(ctx context.Context, ws, prompt, title string) (string, error) {
	if r.failOn != "" && strings.Contains(title, r.failOn) {
		return "", errors.New("job blew up")
	}
	return r.result, nil
}

// hubOnNotify mirrors the wiring cmd/agentd/main.go installs: it maps a job run
// to a Notification and dispatches it to the hub. Keeping the mapping here lets
// the test exercise the real scheduler notify policy end to end.
func hubOnNotify(hub *notify.Hub) func(*scheduler.Job, scheduler.JobRun) {
	return func(job *scheduler.Job, run scheduler.JobRun) {
		lvl := notify.LevelResult
		if run.Status != "ok" {
			lvl = notify.LevelIssue
		}
		hub.Dispatch(notify.Notification{
			Source: "job:" + job.Name,
			Level:  lvl,
			Text:   scheduler.NotifyText(job, run),
			Ts:     run.End,
		})
	}
}

// TestSchedulerNotifyReachesWebSink proves the full path the task asks for:
// scheduler notify policy -> OnNotify -> notify.Hub.Dispatch -> a fake web
// channel (a notify.Sink) receives it. Issues-only SUPPRESSES a clean run;
// always/result DELIVERS it; a failed issues-only run DELIVERS.
func TestSchedulerNotifyReachesWebSink(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "sched.jsonl")
	fr := &fakeRunner{result: "all clean", failOn: "broken"}

	clean := &scheduler.Job{Name: "clean", Enabled: true, Prompt: "p",
		Notify: scheduler.NotifyIssues, Trigger: scheduler.Trigger{Kind: scheduler.TriggerWebhook}}
	always := &scheduler.Job{Name: "always", Enabled: true, Prompt: "p",
		Notify: scheduler.NotifyAlways, Trigger: scheduler.Trigger{Kind: scheduler.TriggerWebhook}}
	broken := &scheduler.Job{Name: "broken", Enabled: true, Prompt: "p",
		Notify: scheduler.NotifyIssues, Trigger: scheduler.Trigger{Kind: scheduler.TriggerWebhook}}

	sched, err := scheduler.New([]*scheduler.Job{clean, always, broken}, fr, statePath, nil)
	if err != nil {
		t.Fatalf("scheduler.New: %v", err)
	}
	defer sched.Close()

	hub := notify.NewHub()
	web := newFakeSink("web") // stands in for the real web channel (also a Sink)
	hub.Register(web)
	sched.OnNotify = hubOnNotify(hub)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sched.Start(ctx)

	for _, name := range []string{"clean", "always", "broken"} {
		if q, reason, err := sched.RunNow(name, "manual"); err != nil || !q {
			t.Fatalf("RunNow(%s): q=%v reason=%q err=%v", name, q, reason, err)
		}
	}

	// Collect notifications for ~1.5s. The clean issues-only run must NOT appear.
	seen := map[string]notify.Notification{}
	deadline := time.After(1500 * time.Millisecond)
loop:
	for len(seen) < 2 {
		select {
		case n := <-web.got:
			seen[n.Source] = n
		case <-deadline:
			break loop
		}
	}

	if n, ok := seen["job:always"]; !ok {
		t.Error("notify=always clean run did not reach the web sink")
	} else if n.Level != notify.LevelResult || !strings.Contains(n.Text, "all clean") {
		t.Errorf("always notification wrong: %+v", n)
	}
	if n, ok := seen["job:broken"]; !ok {
		t.Error("failed issues-only run did not reach the web sink")
	} else if n.Level != notify.LevelIssue || !strings.Contains(n.Text, "FAILED") {
		t.Errorf("broken notification wrong: %+v", n)
	}
	if _, ok := seen["job:clean"]; ok {
		t.Error("clean issues-only run leaked a notification (must be suppressed)")
	}
}

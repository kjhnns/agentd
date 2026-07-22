// Package scheduler is agentd's SCHEDULER / TRIGGER subsystem: it makes the
// agent PROACTIVE (continuous and scheduled jobs), not just reactive to inbound
// messages.
//
// MODEL: a Job is a declaratively configured unit (config [[job]]): a trigger
// (schedule | file | webhook; inbound messages are already a trigger via
// session.RouteInbound), a target WORKSPACE, and a task PROMPT. A job RUN is
// nothing special: agentd creates-or-reuses the warm session in the target
// workspace and Sends the prompt as an ordinary turn. The harness does the work
// with its own shell/file tools against the workspace (running whatever CLIs
// live there); the normal session lifecycle handles events, git autocommit,
// idle reclaim, and context reset. There is NO parallel execution path.
//
// SERIALIZATION: a job run reuses the session homed in its workspace, so
// session.turnMu queues it behind any in-progress interactive turn (never two
// harness turns concurrently in one workspace). Additionally v1 is conservative
// ACROSS workspaces: a single worker goroutine drains the run queue, so at most
// ONE job-driven harness turn is in flight at a time overall (interactive turns
// in other workspaces are unaffected). A fire while the same job is still
// queued/running records a "skipped" run instead of stacking.
//
// DURABILITY: the scheduler appends job_fire / job_skip / job_run / job_notify
// records to its own append-only JSONL state file (fsync per record, same
// discipline as internal/runlog) and rebuilds last-fire times + run history by
// replay on startup, so a restart neither double-fires a just-run schedule nor
// forgets history.
//
// CATCH-UP POLICY (documented default: skip-with-log): occurrences missed while
// the server was down or asleep are NOT fired on startup/wake. Any occurrence
// more than CatchupGrace (default 5m) stale is skipped and a job_skip record is
// logged; only occurrences that come due while running (within the grace) fire.
//
// NOTIFY: policy per job: "issues" (default; surface only non-ok runs),
// "always", "never". A notify is recorded on the run + state log and handed to
// the OnNotify hook, which a channel adapter consumes later. Telegram is NOT
// hard-wired.
package scheduler

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/kjhnns/agentd/internal/config"
	"github.com/kjhnns/agentd/internal/runlog"
)

// NotifyPolicy controls when a job run surfaces to the user.
type NotifyPolicy string

const (
	NotifyIssues NotifyPolicy = "issues" // default: only non-ok runs surface
	NotifyAlways NotifyPolicy = "always"
	NotifyNever  NotifyPolicy = "never"
)

// Job is one declaratively configured proactive job.
type Job struct {
	Name      string
	Enabled   bool
	Trigger   Trigger
	Workspace string // target workspace ("" = the store default)
	Prompt    string // the task told to the harness
	Notify    NotifyPolicy
}

// JobRun is one recorded run (or skip) of a job.
type JobRun struct {
	JobID     string    `json:"job"`
	RunID     string    `json:"run_id"`
	Trigger   string    `json:"trigger"` // schedule | manual | webhook | file
	SchedTime time.Time `json:"sched_time,omitempty"`
	Start     time.Time `json:"start"`
	End       time.Time `json:"end,omitempty"`
	Status    string    `json:"status"` // ok | error | skipped
	Result    string    `json:"result,omitempty"`
	Error     string    `json:"error,omitempty"`
	Notified  bool      `json:"notified"`
}

// JobStatus is the API/CLI list view of a job.
type JobStatus struct {
	Name      string     `json:"name"`
	Enabled   bool       `json:"enabled"`
	Trigger   string     `json:"trigger"`
	Workspace string     `json:"workspace"`
	Notify    string     `json:"notify"`
	NextFire  *time.Time `json:"next_fire,omitempty"`
	LastRun   *JobRun    `json:"last_run,omitempty"`
}

// Runner executes one job turn against a workspace. session-backed in prod
// (ManagerRunner); faked in tests.
type Runner interface {
	RunTurn(ctx context.Context, workspaceName, prompt, title string) (result string, err error)
}

const (
	defaultTick         = time.Second
	defaultTurnTimeout  = 15 * time.Minute
	defaultCatchupGrace = 5 * time.Minute
	defaultFilePoll     = 10 * time.Second
	historyCap          = 50
	runQueueCap         = 16
	resultTruncate      = 500
)

type fireReq struct {
	job       *Job
	trigger   string
	schedTime time.Time
}

// Scheduler owns the clock loop, the run queue + worker, trigger watchers, the
// durable state log, and the in-memory run history ring.
type Scheduler struct {
	runner    Runner
	state     *runlog.Log // durable scheduler state (job_fire/job_skip/job_run/job_notify)
	statePath string
	mirror    *runlog.Log // optional: mirror records into the main run-log

	// Tunables (set before Start).
	Tick         time.Duration // schedule check cadence (default 1s)
	TurnTimeout  time.Duration // bound on one job turn (default 15m)
	CatchupGrace time.Duration // stale-occurrence skip threshold (default 5m)

	// OnNotify is the notify hook a channel adapter consumes later. Nil = the
	// notify is only recorded (run-log + JobRun.Notified).
	OnNotify func(job *Job, run JobRun)

	mu       sync.Mutex
	jobs     map[string]*Job
	order    []string
	lastFire map[string]time.Time // per schedule job: last fired/skipped occurrence
	history  map[string][]JobRun  // ring per job, newest last
	active   map[string]bool      // job currently queued or running
	runq     chan fireReq
}

// New builds a Scheduler over the given jobs, replaying statePath (if it
// exists) to restore last-fire times and run history. mirror may be nil.
func New(jobs []*Job, runner Runner, statePath string, mirror *runlog.Log) (*Scheduler, error) {
	s := &Scheduler{
		runner:    runner,
		statePath: statePath,
		mirror:    mirror,
		jobs:      make(map[string]*Job),
		lastFire:  make(map[string]time.Time),
		history:   make(map[string][]JobRun),
		active:    make(map[string]bool),
		runq:      make(chan fireReq, runQueueCap),
	}
	for _, j := range jobs {
		if _, dup := s.jobs[j.Name]; dup {
			return nil, fmt.Errorf("scheduler: duplicate job name %q", j.Name)
		}
		s.jobs[j.Name] = j
		s.order = append(s.order, j.Name)
	}
	if err := s.replay(); err != nil {
		return nil, err
	}
	st, err := runlog.Open(statePath)
	if err != nil {
		return nil, fmt.Errorf("scheduler state: %w", err)
	}
	s.state = st
	return s, nil
}

// replay rebuilds last-fire times and run history from the state JSONL.
// job_fire and job_skip both advance the schedule position (so a skipped
// occurrence is not re-skipped noisily after every restart); job_run rebuilds
// the history ring.
func (s *Scheduler) replay() error {
	recs, err := runlog.Replay(s.statePath)
	if err != nil {
		return fmt.Errorf("scheduler replay: %w", err)
	}
	for _, r := range recs {
		switch r.Type {
		case "job_fire", "job_skip":
			var d struct {
				Job       string    `json:"job"`
				SchedTime time.Time `json:"sched_time"`
			}
			if err := json.Unmarshal(r.Data, &d); err != nil || d.Job == "" {
				continue
			}
			if d.SchedTime.After(s.lastFire[d.Job]) {
				s.lastFire[d.Job] = d.SchedTime
			}
		case "job_run":
			var run JobRun
			if err := json.Unmarshal(r.Data, &run); err != nil || run.JobID == "" {
				continue
			}
			s.pushHistory(run)
		}
	}
	return nil
}

func (s *Scheduler) pushHistory(run JobRun) {
	h := append(s.history[run.JobID], run)
	if len(h) > historyCap {
		h = h[len(h)-historyCap:]
	}
	s.history[run.JobID] = h
}

func (s *Scheduler) appendState(typ string, payload any) {
	if s.state != nil {
		if err := s.state.Append(typ, payload); err != nil {
			log.Printf("scheduler: state append %s: %v", typ, err)
		}
	}
	if s.mirror != nil {
		_ = s.mirror.Append(typ, payload)
	}
}

func (s *Scheduler) tick() time.Duration {
	if s.Tick > 0 {
		return s.Tick
	}
	return defaultTick
}

func (s *Scheduler) turnTimeout() time.Duration {
	if s.TurnTimeout > 0 {
		return s.TurnTimeout
	}
	return defaultTurnTimeout
}

func (s *Scheduler) catchupGrace() time.Duration {
	if s.CatchupGrace > 0 {
		return s.CatchupGrace
	}
	return defaultCatchupGrace
}

// Start launches the clock loop, the single run worker, and one watcher per
// file-trigger job. It returns immediately; everything stops when ctx ends.
func (s *Scheduler) Start(ctx context.Context) {
	now := time.Now()
	s.mu.Lock()
	for _, name := range s.order {
		j := s.jobs[name]
		if j.Trigger.Kind == TriggerSchedule && s.lastFire[name].IsZero() {
			// Never-fired baseline: schedule positions start at server start,
			// so "@every d" first fires at start+d and cron/daily at the next
			// occurrence after start (no fire-on-startup).
			s.lastFire[name] = now
		}
	}
	s.mu.Unlock()

	go s.loop(ctx)
	go s.worker(ctx)
	for _, name := range s.order {
		j := s.jobs[name]
		if j.Enabled && j.Trigger.Kind == TriggerFile {
			go s.watchLoop(ctx, j)
		}
	}
}

func (s *Scheduler) loop(ctx context.Context) {
	t := time.NewTicker(s.tick())
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.checkDue(time.Now())
		}
	}
}

// checkDue fires every schedule job whose next occurrence has come due,
// applying the skip-with-log catch-up policy for stale occurrences.
func (s *Scheduler) checkDue(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, name := range s.order {
		j := s.jobs[name]
		if !j.Enabled || j.Trigger.Kind != TriggerSchedule || j.Trigger.Schedule == nil {
			continue
		}
		next := j.Trigger.Schedule.Next(s.lastFire[name])
		if next.IsZero() || next.After(now) {
			continue
		}
		// Fast-forward past all but the latest due occurrence.
		missed := 0
		for {
			n2 := j.Trigger.Schedule.Next(next)
			if n2.IsZero() || n2.After(now) {
				break
			}
			next = n2
			missed++
		}
		if now.Sub(next) > s.catchupGrace() {
			// Even the latest due occurrence is stale (downtime/sleep):
			// skip-with-log, advance the schedule position durably.
			missed++
			s.lastFire[name] = next
			s.appendState("job_skip", map[string]any{
				"job": name, "sched_time": next, "missed": missed,
				"reason": "stale occurrence (catch-up policy: skip)",
			})
			log.Printf("scheduler: job %s skipped %d missed occurrence(s) up to %s (catch-up policy: skip)", name, missed, next)
			continue
		}
		if missed > 0 {
			s.appendState("job_skip", map[string]any{
				"job": name, "sched_time": next.Add(-time.Second), "missed": missed,
				"reason": "older occurrences superseded by latest due one",
			})
		}
		s.lastFire[name] = next
		s.appendState("job_fire", map[string]any{"job": name, "sched_time": next, "fired_at": now})
		s.fireLocked(j, "schedule", next)
	}
}

// fireLocked enqueues one run. Caller holds s.mu. Overlap (job already queued
// or running) and a full queue record a "skipped" run instead of stacking.
func (s *Scheduler) fireLocked(j *Job, trigger string, schedTime time.Time) (queued bool, reason string) {
	now := time.Now().UTC()
	if s.active[j.Name] {
		run := JobRun{JobID: j.Name, RunID: newRunID(), Trigger: trigger, SchedTime: schedTime,
			Start: now, End: now, Status: "skipped", Result: "previous run still active"}
		s.recordRunLocked(j, run)
		return false, "previous run still active"
	}
	select {
	case s.runq <- fireReq{job: j, trigger: trigger, schedTime: schedTime}:
		s.active[j.Name] = true
		return true, ""
	default:
		run := JobRun{JobID: j.Name, RunID: newRunID(), Trigger: trigger, SchedTime: schedTime,
			Start: now, End: now, Status: "skipped", Result: "run queue full"}
		s.recordRunLocked(j, run)
		return false, "run queue full"
	}
}

// worker drains the run queue SERIALLY: at most one job-driven harness turn in
// flight at a time overall (the documented conservative v1 choice).
func (s *Scheduler) worker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case req := <-s.runq:
			s.execute(ctx, req)
		}
	}
}

func (s *Scheduler) execute(ctx context.Context, req fireReq) {
	j := req.job
	run := JobRun{JobID: j.Name, RunID: newRunID(), Trigger: req.trigger,
		SchedTime: req.schedTime, Start: time.Now().UTC()}
	s.appendState("job_run_start", map[string]any{"job": j.Name, "run_id": run.RunID, "trigger": req.trigger})
	log.Printf("scheduler: job %s run %s starting (trigger=%s, workspace=%q)", j.Name, run.RunID, req.trigger, j.Workspace)

	rctx, cancel := context.WithTimeout(ctx, s.turnTimeout())
	result, err := s.runner.RunTurn(rctx, j.Workspace, j.Prompt, "job:"+j.Name)
	cancel()

	run.End = time.Now().UTC()
	if err != nil {
		run.Status = "error"
		run.Error = err.Error()
	} else {
		run.Status = "ok"
		run.Result = truncate(result, resultTruncate)
	}

	s.mu.Lock()
	s.active[j.Name] = false
	s.recordRunLocked(j, run)
	s.mu.Unlock()
	log.Printf("scheduler: job %s run %s finished status=%s in %s", j.Name, run.RunID, run.Status, run.End.Sub(run.Start).Round(time.Millisecond))
}

// recordRunLocked stores a finished/skipped run in the ring + durable log and
// applies the notify policy. Caller holds s.mu; the OnNotify hook is invoked
// on its own goroutine so a slow consumer never blocks the scheduler.
func (s *Scheduler) recordRunLocked(j *Job, run JobRun) {
	if shouldNotify(j.Notify, run.Status) {
		run.Notified = true
	}
	s.pushHistory(run)
	s.appendState("job_run", run)
	if run.Notified {
		s.appendState("job_notify", map[string]any{
			"job": j.Name, "run_id": run.RunID, "status": run.Status,
			"text": notifyText(j, run),
		})
		if s.OnNotify != nil {
			go s.OnNotify(j, run)
		}
	}
}

func shouldNotify(p NotifyPolicy, status string) bool {
	switch p {
	case NotifyAlways:
		return true
	case NotifyNever:
		return false
	default: // NotifyIssues ("" included)
		return status != "ok"
	}
}

func notifyText(j *Job, run JobRun) string {
	switch run.Status {
	case "ok":
		return fmt.Sprintf("job %s: %s", j.Name, run.Result)
	case "skipped":
		return fmt.Sprintf("job %s skipped: %s", j.Name, run.Result)
	default:
		return fmt.Sprintf("job %s FAILED: %s", j.Name, run.Error)
	}
}

// RunNow fires a job immediately (manual/webhook/file trigger path). It does
// NOT advance the schedule position, so a manual run never shifts a schedule.
// Returns whether the run was queued (false = recorded as skipped).
func (s *Scheduler) RunNow(name, trigger string) (queued bool, reason string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[name]
	if !ok {
		return false, "", fmt.Errorf("scheduler: unknown job %q", name)
	}
	q, r := s.fireLocked(j, trigger, time.Time{})
	return q, r, nil
}

// Get returns a job by name.
func (s *Scheduler) Get(name string) (*Job, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[name]
	return j, ok
}

// List snapshots every job with its next fire time and last run.
func (s *Scheduler) List() []JobStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]JobStatus, 0, len(s.order))
	for _, name := range s.order {
		j := s.jobs[name]
		st := JobStatus{
			Name: j.Name, Enabled: j.Enabled, Trigger: j.Trigger.String(),
			Workspace: j.Workspace, Notify: string(notifyOrDefault(j.Notify)),
		}
		if j.Trigger.Kind == TriggerSchedule && j.Trigger.Schedule != nil {
			last := s.lastFire[name]
			if last.IsZero() {
				last = time.Now()
			}
			if n := j.Trigger.Schedule.Next(last); !n.IsZero() {
				st.NextFire = &n
			}
		}
		if h := s.history[name]; len(h) > 0 {
			lr := h[len(h)-1]
			st.LastRun = &lr
		}
		out = append(out, st)
	}
	return out
}

func notifyOrDefault(p NotifyPolicy) NotifyPolicy {
	if p == "" {
		return NotifyIssues
	}
	return p
}

// Runs returns the most recent n runs of a job, newest first.
func (s *Scheduler) Runs(name string, n int) ([]JobRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.jobs[name]; !ok {
		return nil, fmt.Errorf("scheduler: unknown job %q", name)
	}
	h := s.history[name]
	if n <= 0 || n > len(h) {
		n = len(h)
	}
	out := make([]JobRun, 0, n)
	for i := len(h) - 1; i >= len(h)-n; i-- {
		out = append(out, h[i])
	}
	return out, nil
}

// Close closes the durable state log.
func (s *Scheduler) Close() error {
	if s.state != nil {
		return s.state.Close()
	}
	return nil
}

// FromConfig converts parsed [[job]] config blocks into scheduler Jobs,
// parsing trigger specs and validating policies.
func FromConfig(cfgJobs []config.Job) ([]*Job, error) {
	var out []*Job
	for i, cj := range cfgJobs {
		if cj.Name == "" {
			return nil, fmt.Errorf("job #%d: name is required", i+1)
		}
		j := &Job{
			Name:      cj.Name,
			Enabled:   cj.Enabled,
			Workspace: cj.Workspace,
			Prompt:    cj.Prompt,
		}
		switch cj.Notify {
		case "", "issues":
			j.Notify = NotifyIssues
		case "always":
			j.Notify = NotifyAlways
		case "never":
			j.Notify = NotifyNever
		default:
			return nil, fmt.Errorf("job %q: notify must be issues|always|never, got %q", cj.Name, cj.Notify)
		}
		switch cj.Trigger {
		case "schedule", "":
			if cj.Schedule == "" {
				return nil, fmt.Errorf("job %q: schedule trigger needs a schedule spec", cj.Name)
			}
			loc := time.Local
			if cj.TZ != "" {
				l, err := time.LoadLocation(cj.TZ)
				if err != nil {
					return nil, fmt.Errorf("job %q: tz %q: %w", cj.Name, cj.TZ, err)
				}
				loc = l
			}
			sched, err := ParseSchedule(cj.Schedule, loc)
			if err != nil {
				return nil, fmt.Errorf("job %q: %w", cj.Name, err)
			}
			j.Trigger = Trigger{Kind: TriggerSchedule, Spec: cj.Schedule, Schedule: sched, TZ: cj.TZ}
		case "file":
			if cj.Path == "" {
				return nil, fmt.Errorf("job %q: file trigger needs a path", cj.Name)
			}
			poll := cj.Poll
			if poll <= 0 {
				poll = defaultFilePoll
			}
			j.Trigger = Trigger{Kind: TriggerFile, Spec: cj.Path, Path: cj.Path, Poll: poll}
		case "webhook":
			j.Trigger = Trigger{Kind: TriggerWebhook}
		case "message":
			return nil, fmt.Errorf("job %q: message triggers are the inbound channel path (session.RouteInbound), not a job", cj.Name)
		default:
			return nil, fmt.Errorf("job %q: unknown trigger kind %q (want schedule|file|webhook)", cj.Name, cj.Trigger)
		}
		if cj.Prompt == "" {
			return nil, fmt.Errorf("job %q: prompt is required", cj.Name)
		}
		out = append(out, j)
	}
	return out, nil
}

func newRunID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

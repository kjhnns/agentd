package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/kjhnns/agentd/internal/eventbus"
	"github.com/kjhnns/agentd/internal/scheduler"
	"github.com/kjhnns/agentd/internal/session"
)

type noopRunner struct{}

func (noopRunner) RunTurn(ctx context.Context, ws, prompt, title string) (string, error) {
	return "ok", nil
}

func jobsServer(t *testing.T) *Server {
	t.Helper()
	bus := eventbus.New()
	mgr := session.NewManager(nil, bus, nil)
	s := New("tok", mgr, bus, func() bool { return true })

	schedule, err := scheduler.ParseSchedule("@every 24h", nil)
	if err != nil {
		t.Fatal(err)
	}

	hook := &scheduler.Job{Name: "deploy-hook", Enabled: true, Prompt: "verify deploy",
		Trigger: scheduler.Trigger{Kind: scheduler.TriggerSchedule, Spec: "@every 24h", Schedule: schedule}}
	sched, err := scheduler.New([]*scheduler.Job{hook}, noopRunner{},
		filepath.Join(t.TempDir(), "sched.jsonl"))
	if err != nil {
		t.Fatalf("scheduler.New: %v", err)
	}
	t.Cleanup(func() { sched.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	sched.Start(ctx)
	s.AttachScheduler(sched)
	return s
}

func doReq(s *Server, method, path, bearer string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// TestJobsAPI: GET /jobs lists jobs, POST /jobs/:name/run queues a manual
// fire, GET /jobs/:name/runs returns history, and all of it sits behind the
// same bearer gate as the rest of the API.
func TestJobsAPI(t *testing.T) {
	s := jobsServer(t)

	if rec := doReq(s, http.MethodGet, "/jobs", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("GET /jobs without bearer = %d, want 401", rec.Code)
	}

	rec := doReq(s, http.MethodGet, "/jobs", "tok")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /jobs = %d: %s", rec.Code, rec.Body)
	}
	var jobs []scheduler.JobStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &jobs); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(jobs) != 1 || jobs[0].Name != "deploy-hook" {
		t.Fatalf("jobs = %+v", jobs)
	}

	if rec := doReq(s, http.MethodPost, "/jobs/deploy-hook/run", "tok"); rec.Code != http.StatusAccepted {
		t.Fatalf("POST /jobs/deploy-hook/run = %d: %s", rec.Code, rec.Body)
	}
	if rec := doReq(s, http.MethodPost, "/jobs/nope/run", "tok"); rec.Code != http.StatusNotFound {
		t.Fatalf("POST /jobs/nope/run = %d, want 404", rec.Code)
	}
	if rec := doReq(s, http.MethodGet, "/jobs/deploy-hook/runs", "tok"); rec.Code != http.StatusOK {
		t.Fatalf("GET /jobs/deploy-hook/runs = %d", rec.Code)
	}
}

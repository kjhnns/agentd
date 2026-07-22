// ManagerRunner executes job runs through the EXISTING session lifecycle: a job
// run is an ordinary turn on the warm session homed in the target workspace
// (create-or-reuse), so git autocommit, context-reset triggers, idle reclaim,
// and turn serialization (session.turnMu) all apply unchanged. There is no
// scheduler-private way to drive the harness.
package scheduler

import (
	"context"
	"time"

	"github.com/kjhnns/agentd/internal/eventbus"
	"github.com/kjhnns/agentd/internal/session"
)

// ManagerRunner is the production Runner over the Session Manager.
type ManagerRunner struct {
	Mgr   *session.Manager
	Bus   *eventbus.Bus
	Model string // default model for job-created sessions
}

// RunTurn creates-or-reuses the session in workspaceName and sends prompt as
// one turn, returning the turn's result text. Reusing the workspace's live
// session means the per-session turn lock queues this run behind any
// in-progress interactive turn (never two harness turns concurrently in one
// workspace); the idle-GC contract means a reclaimed session is lazily
// re-hydrated fresh from the workspace artifacts.
func (r *ManagerRunner) RunTurn(ctx context.Context, workspaceName, prompt, title string) (string, error) {
	s, ok := r.Mgr.FindByWorkspace(workspaceName)
	if !ok {
		var err error
		s, err = r.Mgr.Create(ctx, workspaceName, "", r.Model, title)
		if err != nil {
			return "", err
		}
	}

	// Subscribe BEFORE sending so the turn's result event cannot be missed.
	subID, events := r.Bus.Subscribe()
	defer r.Bus.Unsubscribe(subID)

	turnErr := make(chan error, 1)
	go func() {
		_, err := r.Mgr.Send(ctx, s.ID, prompt)
		turnErr <- err
	}()

	var result string
	for {
		select {
		case e := <-events:
			if e.SessionID == s.ID && e.Kind == eventbus.KindResult {
				result = e.Text
			}
		case err := <-turnErr:
			if err != nil {
				return "", err
			}
			if result == "" {
				// The result event may still be in flight behind Send
				// returning; drain briefly.
				grace := time.After(2 * time.Second)
				for {
					select {
					case e := <-events:
						if e.SessionID == s.ID && e.Kind == eventbus.KindResult {
							return e.Text, nil
						}
					case <-grace:
						return result, nil
					}
				}
			}
			return result, nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
}

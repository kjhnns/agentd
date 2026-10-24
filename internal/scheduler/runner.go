// ManagerRunner executes job runs through the EXISTING session lifecycle: a job
// run is an ordinary turn on the warm session homed in the target workspace
// (create-or-reuse), so git autocommit, context-reset triggers, idle reclaim,
// and turn serialization (session.turnMu) all apply unchanged. There is no
// scheduler-private way to drive the harness.
package scheduler

import (
	"context"
	"errors"

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
	for attempt := 0; ; attempt++ {
		s, ok := r.Mgr.FindByWorkspace(workspaceName)
		if !ok {
			var err error
			s, err = r.Mgr.Create(ctx, workspaceName, "", r.Model, title)
			if err != nil {
				return "", err
			}
		}
		// The turn is owned and collected by session.RunTurn (the one
		// implementation used by the channel router and the HTTP API too). The
		// job run is already bounded by the scheduler's per-job context, so no
		// extra turn timeout is imposed here.
		res, err := r.Mgr.RunTurn(ctx, session.TurnRequest{SessionID: s.ID, Text: prompt, Chat: "job:" + title})
		if errors.Is(err, session.ErrSessionGone) && attempt < 2 {
			// Reclaimed while the run waited: find or create it again.
			continue
		}
		var te *session.TurnTimeoutError
		if err != nil && !errors.As(err, &te) {
			return "", err
		}
		return res.Text, err
	}
}

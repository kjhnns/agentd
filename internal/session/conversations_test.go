package session

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kjhnns/agentd/internal/channel"
	"github.com/kjhnns/agentd/internal/channel/watch"
	"github.com/kjhnns/agentd/internal/eventbus"
	"github.com/kjhnns/agentd/internal/runlog"
)

type convRig struct {
	mgr      *Manager
	harness  *echoAdapter
	wch      *watch.Adapter
	store    *watch.Store
	mainMap  *ChatMap
	sideMap  *ChatMap
	runlog   *runlog.Log
	stateDir string
}

func newConvRig(t *testing.T) *convRig {
	t.Helper()
	root := t.TempDir()
	state := filepath.Join(root, "state")
	rl, err := runlog.Open(filepath.Join(state, "runlog.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rl.Close() })
	st, err := watch.OpenStore(filepath.Join(state, "watch-messages.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	wch := watch.New("wt", st)
	if err := wch.WithArtifacts(filepath.Join(state, "watch-artifacts"), filepath.Join(root, "workspaces")); err != nil {
		t.Fatal(err)
	}
	a := newEchoAdapter()
	mgr := NewManager(a, eventbus.New(), rl)
	mgr.Policy = Policy{TurnTimeout: 5 * time.Second}
	return &convRig{mgr: mgr, harness: a, wch: wch, store: st, mainMap: NewChatMap(LaneMain), sideMap: NewChatMap(LaneSide), runlog: rl, stateDir: state}
}

// post sends one message through the HTTP surface and returns its inbound.
func (r *convRig) post(t *testing.T, body string) channel.InboundMsg {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/watch/messages", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer wt")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.wch.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("post %s: %d %s", body, rec.Code, rec.Body.String())
	}
	select {
	case in := <-r.wch.Inbound():
		return in
	case <-time.After(time.Second):
		t.Fatal("nothing queued")
	}
	return channel.InboundMsg{}
}

func (r *convRig) route(t *testing.T, in channel.InboundMsg) {
	t.Helper()
	if err := r.mgr.RouteConversation(context.Background(), r.wch, in, r.mainMap, r.sideMap, t.TempDir(), "", r.wch); err != nil {
		t.Fatalf("route %q: %v", in.Text, err)
	}
}

func (r *convRig) lastSend() string {
	r.harness.mu.Lock()
	defer r.harness.mu.Unlock()
	return r.harness.sends[len(r.harness.sends)-1]
}

// TestContinueAnOldConversationAttachesItsTranscript: switching back to a
// side conversation that already has messages starts fresh with its own
// transcript attached; the turn text holds only the pointer line, never the
// transcript; the store row shows the attachment without the path.
func TestContinueAnOldConversationAttachesItsTranscript(t *testing.T) {
	r := newConvRig(t)
	r.route(t, r.post(t, `{"text":"alpha question","conv":"a"}`))
	if strings.Contains(r.lastSend(), "Attached earlier conversation") {
		t.Fatal("a brand-new conversation got an artifact")
	}
	r.route(t, r.post(t, `{"text":"bravo question","conv":"b"}`))
	in := r.post(t, `{"text":"back to alpha","conv":"a"}`)
	r.route(t, in)

	sent := r.lastSend()
	first, rest, _ := strings.Cut(sent, "\n\n")
	if !strings.HasPrefix(first, `Attached earlier conversation "alpha question" (2 messages, `) || !strings.Contains(first, filepath.Join(r.stateDir, "watch-artifacts")) {
		t.Fatalf("pointer line = %q", first)
	}
	if rest != "back to alpha" {
		t.Fatalf("turn text after the pointer = %q", rest)
	}
	if strings.Contains(sent, "re:alpha question") || strings.Contains(sent, "bravo") {
		t.Fatalf("the turn carries a transcript:\n%s", sent)
	}
	m, _ := r.store.Get(in.MsgID)
	if len(m.Attachments) != 1 || m.Attachments[0].ID != "a" || m.Attachments[0].Path == "" || m.Public().Attachments[0].Path != "" {
		t.Fatalf("attachments = %+v", m.Attachments)
	}
	// The conversation's own answer is filed into it, not into main.
	tail, _, _ := r.store.TailF(watch.InConv("a"), 50)
	if last := tail[len(tail)-1]; last.Role != watch.RoleAgent || last.Conv != "a" {
		t.Fatalf("answer = %+v", last)
	}
	if main, _, _ := r.store.TailF(watch.MainOnly, 50); len(main) != 0 {
		t.Fatalf("main got %d side rows", len(main))
	}
	// One side session served all three; main has none.
	if _, ok := r.mainMap.Get(watch.DefaultUserID); ok {
		t.Fatal("a side message created a main session")
	}
	if _, ok := r.sideMap.Get(SideChat); !ok {
		t.Fatal("no side session")
	}
	if got := r.mgr.conversationEpoch(SideChat); got != 3 {
		t.Fatalf("epoch = %d after a, b, a", got)
	}
}

// TestContextConvAttachesAnotherConversation: context_conv works in main
// too; the pointer goes before the text and the turn record keeps it.
func TestContextConvAttachesAnotherConversation(t *testing.T) {
	r := newConvRig(t)
	r.route(t, r.post(t, `{"text":"side facts","conv":"s"}`))
	r.route(t, r.post(t, `{"text":"use those","context_conv":"s"}`))
	sent := r.lastSend()
	if !strings.HasPrefix(sent, `Attached earlier conversation "side facts" (2 messages, `) || !strings.HasSuffix(sent, "\n\nuse those") {
		t.Fatalf("turn = %q", sent)
	}
	recs, _ := runlog.Replay(r.runlog.Path())
	var tr TurnRecord
	for _, rec := range recs {
		if rec.Type == "turn" {
			_ = json.Unmarshal(rec.Data, &tr)
		}
	}
	if tr.Conv != ConvMain || tr.UserText != "use those" || len(tr.Attachments) != 1 || tr.Attachments[0].Kind != "conv" {
		t.Fatalf("turn record = %+v", tr)
	}
	if _, ok := r.mainMap.Get(watch.DefaultUserID); !ok {
		t.Fatal("main message did not use the main session")
	}
}

// TestSideAndMainTurnsNeverOverlapThroughRouting: a side message waits for
// a running main turn (one workspace, one interactive lock).
func TestSideAndMainTurnsNeverOverlapThroughRouting(t *testing.T) {
	r := newConvRig(t)
	gate := r.harness.gate("main work")
	mainIn := r.post(t, `{"text":"main work"}`)
	sideIn := r.post(t, `{"text":"side work","conv":"x"}`)
	done := make(chan struct{}, 2)
	cwd := t.TempDir()
	go func() {
		_ = r.mgr.RouteConversation(context.Background(), r.wch, mainIn, r.mainMap, r.sideMap, cwd, "", r.wch)
		done <- struct{}{}
	}()
	waitFor(t, func() bool { return r.harness.sendsContaining("main work") == 1 })
	go func() {
		_ = r.mgr.RouteConversation(context.Background(), r.wch, sideIn, r.mainMap, r.sideMap, cwd, "", r.wch)
		done <- struct{}{}
	}()
	time.Sleep(200 * time.Millisecond)
	if r.harness.sendsContaining("side work") != 0 {
		t.Fatal("a side turn ran beside a main turn")
	}
	close(gate)
	<-done
	<-done
	if r.harness.sendsContaining("side work") != 1 {
		t.Fatal("the side turn did not run after the main one")
	}
}

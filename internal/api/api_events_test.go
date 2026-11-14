package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kjhnns/agentd/internal/eventbus"
	"github.com/kjhnns/agentd/internal/session"
	"github.com/kjhnns/agentd/internal/wsutil"
)

func TestEventsDisconnectUnsubscribes(t *testing.T) {
	bus := eventbus.New()
	mgr := session.NewManager(replyAdapter{}, bus, nil)
	mgr.Policy = session.Policy{}
	sess, err := mgr.Create(context.Background(), "", t.TempDir(), "", "cli:test")
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Teardown(sess.ID)
	srv := httptest.NewServer(New("tok", mgr, bus, nil).Handler())
	defer srv.Close()
	before := bus.SubscriberCount()
	conn, err := wsutil.Dial("ws"+strings.TrimPrefix(srv.URL, "http")+"/sessions/"+sess.ID+"/events", http.Header{"Authorization": {"Bearer tok"}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	waitCount := func(want int) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if bus.SubscriberCount() == want {
				return
			}
			time.Sleep(time.Millisecond)
		}
		t.Fatalf("subscriber count = %d, want %d", bus.SubscriberCount(), want)
	}
	waitCount(before + 1)
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	waitCount(before)
}

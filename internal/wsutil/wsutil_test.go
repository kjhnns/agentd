package wsutil_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kjhnns/agentd/internal/wsutil"
)

// TestUpgradeDialRoundTrip: a client dials, sends a masked text frame, the server
// reads it (unmasks), echoes it back unmasked, and the client reads it. This
// exercises both directions of the frame codec (client masking + server read,
// server write + client read).
func TestUpgradeDialRoundTrip(t *testing.T) {
	got := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := wsutil.Upgrade(w, r)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer conn.Close()
		msg, err := conn.ReadText()
		if err != nil {
			t.Errorf("server read: %v", err)
			return
		}
		got <- string(msg)
		_ = conn.WriteText([]byte("echo:" + string(msg)))
		// give the client time to read before the handler returns/closes.
		time.Sleep(100 * time.Millisecond)
	}))
	defer srv.Close()

	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"
	conn, err := wsutil.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	if err := conn.WriteText([]byte("hello")); err != nil {
		t.Fatalf("client write: %v", err)
	}
	select {
	case s := <-got:
		if s != "hello" {
			t.Fatalf("server got %q, want hello", s)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server never received the client frame")
	}
	reply, err := conn.ReadText()
	if err != nil {
		t.Fatalf("client read: %v", err)
	}
	if string(reply) != "echo:hello" {
		t.Fatalf("client got %q, want echo:hello", reply)
	}
}

// TestDialRejectsNonWS: a plain HTTP endpoint (no upgrade) fails the handshake.
func TestDialRejectsNonWS(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/"
	if _, err := wsutil.Dial(url, nil); err == nil {
		t.Fatal("expected handshake to fail against a non-websocket endpoint")
	}
}

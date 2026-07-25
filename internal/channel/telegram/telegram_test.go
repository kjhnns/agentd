package telegram

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kjhnns/agentd/internal/channel"
)

func TestAllowlistEnforcement(t *testing.T) {
	a := New("tok", []string{"111"})
	if !a.allowed("111") {
		t.Error("111 should be allowed")
	}
	if a.allowed("999") {
		t.Error("999 should be rejected")
	}

	// Allowlisted message is emitted; non-allowlisted is dropped.
	a.handleUpdate(tgUpdate{UpdateID: 1, Message: msgFrom(111, "hi")})
	a.handleUpdate(tgUpdate{UpdateID: 2, Message: msgFrom(999, "sneaky")})

	select {
	case m := <-a.Inbound():
		if m.UserID != "111" || m.Text != "hi" {
			t.Fatalf("got %+v", m)
		}
	default:
		t.Fatal("expected an inbound message from allowlisted chat")
	}
	select {
	case m := <-a.Inbound():
		t.Fatalf("did not expect a second message, got %+v", m)
	default:
	}
	if a.offset != 3 {
		t.Errorf("offset = %d, want 3", a.offset)
	}
}

func msgFrom(chatID int64, text string) *tgMessage {
	m := &tgMessage{Text: text}
	m.Chat.ID = chatID
	return m
}

func TestSendMessage(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/sendMessage") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		buf := make([]byte, r.ContentLength)
		r.Body.Read(buf)
		gotBody = string(buf)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true,"result":{"message_id":42}}`))
	}))
	defer srv.Close()

	a := New("tok", []string{"111"})
	a.base = srv.URL

	rcpt, err := a.Send(context.Background(), channel.OutboundMsg{ChatID: "111", Text: "hello"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if rcpt.ID != "42" {
		t.Errorf("receipt id = %q, want 42", rcpt.ID)
	}
	if !strings.Contains(gotBody, "hello") {
		t.Errorf("request body missing text: %s", gotBody)
	}
}

func TestSendMediaStubbed(t *testing.T) {
	a := New("tok", []string{"111"})
	_, err := a.Send(context.Background(), channel.OutboundMsg{ChatID: "111", Media: []string{"x.jpg"}})
	if err == nil {
		t.Fatal("expected media send to be a stub error")
	}
}

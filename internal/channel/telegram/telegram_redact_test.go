package telegram

import (
	"bytes"
	"context"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kjhnns/agentd/internal/channel"
)

// testToken has the real Bot API shape (<botid>:<secret>) so a substring match
// is meaningful. The secret half is what must never survive into a log or error.
const testToken = "8286910814:AAFAKEfakeFAKEtestONLYtoken0123456789"

// assertScrubbed fails if the token (or its secret half) shows up anywhere in s.
func assertScrubbed(t *testing.T, what, s string) {
	t.Helper()
	if s == "" {
		t.Fatalf("%s: expected a non-empty message to inspect", what)
	}
	if strings.Contains(s, testToken) {
		t.Errorf("%s LEAKS THE FULL TOKEN: %s", what, s)
	}
	secret := testToken[strings.Index(testToken, ":")+1:]
	if strings.Contains(s, secret) {
		t.Errorf("%s LEAKS THE TOKEN SECRET: %s", what, s)
	}
}

// newDeadAdapter points the adapter at a closed port so every Bot API call
// fails inside http.Client.Do, producing the *url.Error that embeds the full
// request URL. This is the exact shape of the real leak (a getUpdates
// "context deadline exceeded" whose url.Error carried bot<TOKEN> into the log).
func newDeadAdapter() *Adapter {
	a := New(testToken, []string{"7597951120"})
	a.base = "http://127.0.0.1:1" // nothing listens on port 1: connection refused
	a.client = &http.Client{Timeout: 2 * time.Second}
	return a
}

// TestGetUpdatesTransportErrorIsRedacted is the regression test for the leak:
// a failed getUpdates must not return the token in its error string.
func TestGetUpdatesTransportErrorIsRedacted(t *testing.T) {
	a := newDeadAdapter()
	_, err := a.getUpdates(context.Background())
	if err == nil {
		t.Fatal("expected a transport error from the dead endpoint")
	}
	assertScrubbed(t, "getUpdates error", err.Error())
	if !strings.Contains(err.Error(), "<redacted>") {
		t.Errorf("expected the redaction marker in %q", err.Error())
	}
}

// TestPollLoopLogLineIsRedacted covers the actual logging boundary: the line
// "telegram: getUpdates error: ..." in pollLoop is what wrote the token to
// logs/agentd.log during the 502/timeout storms.
func TestPollLoopLogLineIsRedacted(t *testing.T) {
	var buf bytes.Buffer
	orig := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(orig)

	a := newDeadAdapter()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { a.pollLoop(ctx); close(done) }()

	// Wait for at least one error line, then stop the loop.
	deadline := time.After(10 * time.Second)
	for buf.Len() == 0 {
		select {
		case <-deadline:
			cancel()
			<-done
			t.Fatal("pollLoop never logged an error")
		case <-time.After(20 * time.Millisecond):
		}
	}
	cancel()
	<-done

	out := buf.String()
	if !strings.Contains(out, "getUpdates error") {
		t.Fatalf("expected a getUpdates error line, got: %s", out)
	}
	assertScrubbed(t, "pollLoop log output", out)
}

// TestSendTransportErrorIsRedacted covers the sendMessage path.
func TestSendTransportErrorIsRedacted(t *testing.T) {
	a := newDeadAdapter()
	_, err := a.Send(context.Background(), channel.OutboundMsg{ChatID: "7597951120", Text: "hi"})
	if err == nil {
		t.Fatal("expected a transport error")
	}
	assertScrubbed(t, "Send error", err.Error())
}

// TestSetReactionTransportErrorIsRedacted covers setMessageReaction, whose
// failures are logged by reactLoop on every rejected emoji.
func TestSetReactionTransportErrorIsRedacted(t *testing.T) {
	a := newDeadAdapter()
	err := a.setReaction("7597951120", 42, "👀")
	if err == nil {
		t.Fatal("expected a transport error")
	}
	assertScrubbed(t, "setReaction error", err.Error())
}

// TestDownloadFileTransportErrorIsRedacted covers the media getFile path.
func TestDownloadFileTransportErrorIsRedacted(t *testing.T) {
	a := newDeadAdapter()
	_, err := a.downloadFile(context.Background(), "FILE123")
	if err == nil {
		t.Fatal("expected a transport error")
	}
	assertScrubbed(t, "downloadFile getFile error", err.Error())
}

// TestDownloadFileDownloadLegIsRedacted covers the SECOND leg of the media
// path: getFile succeeds, then the CDN download URL (which also embeds the
// token, /file/bot<TOKEN>/...) fails.
func TestDownloadFileDownloadLegIsRedacted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/getFile") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true,"result":{"file_path":"voice/file_1.oga"}}`))
			return
		}
		// The download leg: hijack and drop so client.Do errors with a url.Error.
		hj, ok := w.(http.Hijacker)
		if !ok {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		conn, _, err := hj.Hijack()
		if err == nil {
			_ = conn.Close()
		}
	}))
	defer srv.Close()

	a := New(testToken, []string{"7597951120"})
	a.base = srv.URL
	a.client = &http.Client{Timeout: 2 * time.Second}

	_, err := a.downloadFile(context.Background(), "FILE123")
	if err == nil {
		t.Fatal("expected the download leg to fail")
	}
	assertScrubbed(t, "downloadFile download-leg error", err.Error())
}

// TestRedactEmptyTokenDoesNotCorrupt guards the ReplaceAll empty-needle trap:
// an adapter with no token must pass strings through untouched.
func TestRedactEmptyTokenDoesNotCorrupt(t *testing.T) {
	a := New("", nil)
	const s = "plain message"
	if got := a.redact(s); got != s {
		t.Errorf("empty-token redact corrupted the string: %q", got)
	}
	if got := a.redactErr(nil); got != nil {
		t.Errorf("redactErr(nil) = %v, want nil", got)
	}
}

package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kjhnns/agentd/internal/media"
)

// stubTranscriber for the voice fixture (no network).
type stubTranscriber struct {
	out media.Transcript
	err error
}

func (s *stubTranscriber) Transcribe(ctx context.Context, path string) (media.Transcript, error) {
	return s.out, s.err
}

// botServer fakes the three Bot API endpoints the media path touches:
// getFile, the file download host path, and sendMessage (captured).
type botServer struct {
	*httptest.Server
	mu        sync.Mutex
	sent      []string // sendMessage text bodies
	fileBytes []byte
	gotFileID string
}

func newBotServer(t *testing.T, fileBytes []byte) *botServer {
	t.Helper()
	bs := &botServer{fileBytes: fileBytes}
	bs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/getFile"):
			bs.mu.Lock()
			bs.gotFileID = r.URL.Query().Get("file_id")
			bs.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true,"result":{"file_path":"documents/remote-file.bin"}}`))
		case strings.Contains(r.URL.Path, "/file/bot"):
			_, _ = w.Write(bs.fileBytes)
		case strings.HasSuffix(r.URL.Path, "/sendMessage"):
			body, _ := io.ReadAll(r.Body)
			var payload struct {
				Text string `json:"text"`
			}
			_ = json.Unmarshal(body, &payload)
			bs.mu.Lock()
			bs.sent = append(bs.sent, payload.Text)
			bs.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":7}}`))
		default:
			t.Errorf("unexpected bot API path: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(bs.Server.Close)
	return bs
}

func (bs *botServer) sentTexts() []string {
	bs.mu.Lock()
	defer bs.mu.Unlock()
	out := make([]string, len(bs.sent))
	copy(out, bs.sent)
	return out
}

func (bs *botServer) waitSent(t *testing.T, n int) []string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := bs.sentTexts(); len(got) >= n {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d sendMessage call(s); got %v", n, bs.sentTexts())
	return nil
}

func newMediaAdapter(t *testing.T, bs *botServer, tr media.Transcriber) (*Adapter, *media.Service) {
	t.Helper()
	svc := &media.Service{Dir: t.TempDir(), Transcriber: tr}
	a := New("tok", []string{"111"}).WithMedia(svc)
	a.base = bs.URL
	return a, svc
}

func voiceMsg(chatID int64, dur int, size int64, caption string) *tgMessage {
	m := &tgMessage{MessageID: 5, Caption: caption,
		Voice: &tgVoice{FileID: "VOICE1", Duration: dur, MimeType: "audio/ogg", FileSize: size}}
	m.Chat.ID = chatID
	return m
}

func TestVoiceUpdateTranscribed(t *testing.T) {
	bs := newBotServer(t, []byte("OPUSDATA"))
	a, _ := newMediaAdapter(t, bs, &stubTranscriber{out: media.Transcript{Text: "hallo joe", Language: "german", DurationS: 4}})

	a.handleUpdate(tgUpdate{UpdateID: 9, Message: voiceMsg(111, 4, 100, "")})

	select {
	case in := <-a.Inbound():
		if len(in.Media) != 1 {
			t.Fatalf("media artifacts = %d, want 1 (%+v)", len(in.Media), in)
		}
		art := in.Media[0]
		if art.Kind != media.KindAudio || art.Transcript != "hallo joe" || art.DurationS != 4 {
			t.Errorf("artifact = %+v", art)
		}
		if art.Path != "" {
			t.Errorf("audio path = %q, want empty (deleted after transcription)", art.Path)
		}
		if in.Text != "" {
			t.Errorf("text = %q, want empty (no caption)", in.Text)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no inbound emitted for voice update")
	}
	if bs.gotFileID != "VOICE1" {
		t.Errorf("getFile file_id = %q, want VOICE1", bs.gotFileID)
	}
	if a.offset != 10 {
		t.Errorf("offset = %d, want 10", a.offset)
	}
}

func TestVoiceTranscribeFailurePoliteReply(t *testing.T) {
	bs := newBotServer(t, []byte("OPUSDATA"))
	a, _ := newMediaAdapter(t, bs, &stubTranscriber{err: errors.New("whisper down")})

	a.processMedia(context.Background(), voiceMsg(111, 4, 100, ""), "111")

	select {
	case in := <-a.Inbound():
		t.Fatalf("unexpected inbound on transcribe failure: %+v", in)
	default:
	}
	sent := bs.waitSent(t, 1)
	if !strings.Contains(sent[0], "transcribe") {
		t.Errorf("reply = %q, want transcription apology", sent[0])
	}
}

func TestPhotoLargestPickedWithCaption(t *testing.T) {
	bs := newBotServer(t, []byte("JPEGDATA"))
	a, svc := newMediaAdapter(t, bs, nil)

	m := &tgMessage{MessageID: 6, Caption: "what is this?",
		Photo: []tgPhotoSize{
			{FileID: "SMALL", Width: 90, Height: 90, FileSize: 1000},
			{FileID: "BIG", Width: 1280, Height: 1280, FileSize: 90000},
			{FileID: "MID", Width: 320, Height: 320, FileSize: 20000},
		}}
	m.Chat.ID = 111

	a.processMedia(context.Background(), m, "111")

	select {
	case in := <-a.Inbound():
		if bs.gotFileID != "BIG" {
			t.Errorf("downloaded file_id = %q, want BIG (largest)", bs.gotFileID)
		}
		if in.Text != "what is this?" {
			t.Errorf("caption not carried: %q", in.Text)
		}
		if len(in.Media) != 1 || in.Media[0].Kind != media.KindImage {
			t.Fatalf("media = %+v", in.Media)
		}
		data, err := os.ReadFile(in.Media[0].Path)
		if err != nil || string(data) != "JPEGDATA" {
			t.Errorf("image not on disk: %q %v", data, err)
		}
		if !strings.Contains(in.Media[0].Path, svc.Dir) {
			t.Errorf("image outside media dir: %s", in.Media[0].Path)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no inbound for photo")
	}
}

func TestDocumentUpdate(t *testing.T) {
	bs := newBotServer(t, []byte("%PDF-1.4"))
	a, _ := newMediaAdapter(t, bs, nil)

	m := &tgMessage{MessageID: 7,
		Document: &tgDocument{FileID: "DOC1", FileName: "report.pdf", MimeType: "application/pdf", FileSize: 8}}
	m.Chat.ID = 111

	a.processMedia(context.Background(), m, "111")

	select {
	case in := <-a.Inbound():
		if len(in.Media) != 1 || in.Media[0].Kind != media.KindDocument {
			t.Fatalf("media = %+v", in.Media)
		}
		if in.Media[0].Name != "report.pdf" || in.Media[0].Mime != "application/pdf" {
			t.Errorf("artifact = %+v", in.Media[0])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no inbound for document")
	}
}

func TestOversizePoliteReplyNoDownload(t *testing.T) {
	bs := newBotServer(t, nil)
	a, svc := newMediaAdapter(t, bs, nil)
	svc.MaxBytes = 1 << 20 // 1 MB cap

	a.processMedia(context.Background(), voiceMsg(111, 60, 5<<20, ""), "111")

	sent := bs.waitSent(t, 1)
	if !strings.Contains(sent[0], "too large") {
		t.Errorf("reply = %q, want too-large notice", sent[0])
	}
	if bs.gotFileID != "" {
		t.Errorf("getFile was called (file_id %q) despite declared oversize", bs.gotFileID)
	}
	select {
	case in := <-a.Inbound():
		t.Fatalf("unexpected inbound: %+v", in)
	default:
	}
}

func TestUnsupportedKindsDecline(t *testing.T) {
	bs := newBotServer(t, nil)
	a, _ := newMediaAdapter(t, bs, nil)

	m := &tgMessage{MessageID: 8, Sticker: &tgFileRef{FileID: "STICK"}}
	m.Chat.ID = 111
	a.handleUpdate(tgUpdate{UpdateID: 20, Message: m}) // goroutine path

	sent := bs.waitSent(t, 1)
	if !strings.Contains(strings.ToLower(sent[0]), "sorry") {
		t.Errorf("decline reply = %q", sent[0])
	}
	select {
	case in := <-a.Inbound():
		t.Fatalf("unexpected inbound for sticker: %+v", in)
	default:
	}
}

func TestMediaWithoutServiceDeclines(t *testing.T) {
	bs := newBotServer(t, nil)
	a := New("tok", []string{"111"}) // no WithMedia
	a.base = bs.URL

	a.processMedia(context.Background(), voiceMsg(111, 3, 10, ""), "111")

	sent := bs.waitSent(t, 1)
	if !strings.Contains(sent[0], "not enabled") {
		t.Errorf("reply = %q, want not-enabled notice", sent[0])
	}
}

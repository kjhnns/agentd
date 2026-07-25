package media

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// stubTranscriber lets tests drive success and failure without any network.
type stubTranscriber struct {
	out   Transcript
	err   error
	calls int
	paths []string
}

func (s *stubTranscriber) Transcribe(ctx context.Context, path string) (Transcript, error) {
	s.calls++
	s.paths = append(s.paths, path)
	return s.out, s.err
}

func TestIngestImage(t *testing.T) {
	svc := &Service{Dir: t.TempDir()}
	art, err := svc.Ingest(context.Background(), Request{
		Reader: strings.NewReader("PNGDATA"), Filename: "pic.png", Mime: "image/png", Source: "telegram",
	})
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if art.Kind != KindImage {
		t.Errorf("kind = %s, want image", art.Kind)
	}
	if art.Size != 7 {
		t.Errorf("size = %d, want 7", art.Size)
	}
	if !strings.Contains(art.Path, filepath.Join("telegram")) {
		t.Errorf("path %s not under source subdir", art.Path)
	}
	data, err := os.ReadFile(art.Path)
	if err != nil || string(data) != "PNGDATA" {
		t.Errorf("stored file mismatch: %q %v", data, err)
	}
	if art.ID == "" || art.CreatedAt.IsZero() {
		t.Errorf("artifact missing id/timestamp: %+v", art)
	}
}

func TestIngestDocument(t *testing.T) {
	svc := &Service{Dir: t.TempDir()}
	art, err := svc.Ingest(context.Background(), Request{
		Reader: strings.NewReader("%PDF-1.4 fake"), Filename: "invoice März.pdf",
		Mime: "application/pdf", Source: "web",
	})
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if art.Kind != KindDocument {
		t.Errorf("kind = %s, want document", art.Kind)
	}
	// Sanitized name keeps extension, no spaces/umlauts.
	if strings.ContainsAny(art.Name, " ä") || !strings.HasSuffix(art.Name, ".pdf") {
		t.Errorf("name not sanitized: %q", art.Name)
	}
	if _, err := os.Stat(art.Path); err != nil {
		t.Errorf("document not on disk: %v", err)
	}
}

func TestIngestAudioTranscribedAndDeleted(t *testing.T) {
	st := &stubTranscriber{out: Transcript{Text: "hello world", Language: "english", DurationS: 7}}
	svc := &Service{Dir: t.TempDir(), Transcriber: st}
	art, err := svc.Ingest(context.Background(), Request{
		Reader: strings.NewReader("OGGDATA"), Filename: "voice-1.oga", Mime: "audio/ogg",
		Source: "telegram", DurationS: 5,
	})
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if art.Kind != KindAudio || art.Transcript != "hello world" {
		t.Fatalf("artifact = %+v", art)
	}
	if art.DurationS != 5 { // source-supplied duration wins over whisper's
		t.Errorf("duration = %d, want 5", art.DurationS)
	}
	if st.calls != 1 {
		t.Errorf("transcriber calls = %d, want 1", st.calls)
	}
	// The audio file is DELETED after successful transcription.
	if art.Path != "" {
		t.Errorf("audio artifact path = %q, want empty (file deleted)", art.Path)
	}
	if _, err := os.Stat(st.paths[0]); !os.IsNotExist(err) {
		t.Errorf("audio file still on disk after transcription: %v", err)
	}
}

func TestIngestAudioWhisperDurationFallback(t *testing.T) {
	st := &stubTranscriber{out: Transcript{Text: "x", DurationS: 42}}
	svc := &Service{Dir: t.TempDir(), Transcriber: st}
	art, err := svc.Ingest(context.Background(), Request{
		Reader: strings.NewReader("A"), Filename: "a.wav", Mime: "audio/wav", Source: "web",
	})
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if art.DurationS != 42 {
		t.Errorf("duration = %d, want 42 (whisper fallback)", art.DurationS)
	}
}

func TestIngestAudioTranscribeFailureKeepsFile(t *testing.T) {
	st := &stubTranscriber{err: errors.New("api down")}
	svc := &Service{Dir: t.TempDir(), Transcriber: st}
	art, err := svc.Ingest(context.Background(), Request{
		Reader: strings.NewReader("OGGDATA"), Filename: "v.oga", Mime: "audio/ogg", Source: "telegram",
	})
	if !errors.Is(err, ErrTranscribe) {
		t.Fatalf("err = %v, want ErrTranscribe", err)
	}
	if art.Path == "" {
		t.Fatal("artifact path empty; file reference lost on failure")
	}
	if _, statErr := os.Stat(art.Path); statErr != nil {
		t.Errorf("audio file not kept on transcribe failure: %v", statErr)
	}
}

func TestIngestAudioNoTranscriberTypedFailure(t *testing.T) {
	svc := &Service{Dir: t.TempDir()}
	_, err := svc.Ingest(context.Background(), Request{
		Reader: strings.NewReader("OGG"), Filename: "v.oga", Mime: "audio/ogg", Source: "telegram",
	})
	if !errors.Is(err, ErrTranscribe) {
		t.Fatalf("err = %v, want ErrTranscribe (voice wired but disabled)", err)
	}
}

func TestIngestTooLarge(t *testing.T) {
	svc := &Service{Dir: t.TempDir(), MaxBytes: 10}
	_, err := svc.Ingest(context.Background(), Request{
		Reader: strings.NewReader(strings.Repeat("x", 11)), Filename: "big.png",
		Mime: "image/png", Source: "web",
	})
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
	// No partial file left behind.
	entries, _ := os.ReadDir(filepath.Join(svc.Dir, "web"))
	if len(entries) != 0 {
		t.Errorf("partial file left after too-large rejection: %v", entries)
	}
}

func TestIngestUnsupportedVideo(t *testing.T) {
	svc := &Service{Dir: t.TempDir()}
	_, err := svc.Ingest(context.Background(), Request{
		Reader: strings.NewReader("MP4"), Filename: "clip.mp4", Mime: "video/mp4", Source: "web",
	})
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("err = %v, want ErrUnsupported", err)
	}
}

func TestKindFor(t *testing.T) {
	cases := []struct {
		name, mime string
		kind       Kind
		ok         bool
	}{
		{"a.jpg", "", KindImage, true},
		{"a.bin", "image/webp", KindImage, true},
		{"v.oga", "", KindAudio, true},
		{"v.bin", "audio/mpeg", KindAudio, true},
		{"r.pdf", "application/pdf", KindDocument, true},
		{"noext", "", KindDocument, true},
		{"c.mp4", "video/mp4", "", false},
		{"c.mov", "", "", false},
	}
	for _, c := range cases {
		k, ok := KindFor(c.name, c.mime)
		if k != c.kind || ok != c.ok {
			t.Errorf("KindFor(%q,%q) = %v,%v want %v,%v", c.name, c.mime, k, ok, c.kind, c.ok)
		}
	}
}

func TestSweepRemovesOldKeepsNew(t *testing.T) {
	dir := t.TempDir()
	svc := &Service{Dir: dir, Retention: 24 * time.Hour}
	sub := filepath.Join(dir, "telegram")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	oldFile := filepath.Join(sub, "old.jpg")
	newFile := filepath.Join(sub, "new.jpg")
	for _, p := range []string{oldFile, newFile} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(oldFile, past, past); err != nil {
		t.Fatal(err)
	}

	if got := svc.Sweep(); got != 1 {
		t.Fatalf("Sweep removed %d, want 1", got)
	}
	if _, err := os.Stat(oldFile); !os.IsNotExist(err) {
		t.Error("old file survived the sweep")
	}
	if _, err := os.Stat(newFile); err != nil {
		t.Error("new file was swept")
	}
}

func TestEnsureGitignoreLine(t *testing.T) {
	root := t.TempDir()
	if err := EnsureGitignoreLine(root, "work/media/"); err != nil {
		t.Fatal(err)
	}
	if err := EnsureGitignoreLine(root, "work/media/"); err != nil { // idempotent
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, ".gitignore"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(data), "work/media/"); got != 1 {
		t.Errorf("gitignore has %d entries, want exactly 1:\n%s", got, data)
	}
}

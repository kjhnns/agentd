// Package media is agentd's CORE multimodal media service, deliberately
// channel-agnostic (design decision 2026-07: media is a core capability, not a
// per-channel feature). Channel adapters only do ACQUISITION (download the
// bytes from their provider) and hand a Reader to Ingest; the service owns the
// storage layout and naming under the workspace's work/media/<source>/ dir,
// the size cap, kind detection, voice transcription (via a pluggable
// Transcriber, see transcribe.go), typed failures, retention sweeping, and
// run-log records. The session layer then renders artifact references into the
// canonical turn text (session.RenderInbound); adapters MUST NOT pre-bake
// those markers.
package media

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kjhnns/agentd/internal/runlog"
)

// Kind classifies an ingested artifact.
type Kind string

const (
	KindImage    Kind = "image"
	KindAudio    Kind = "audio"
	KindDocument Kind = "document"
)

// Typed failures (design: too-large / unsupported / transcribe-failed are the
// three failure classes a channel adapter turns into a polite reply). Match
// with errors.Is.
var (
	ErrTooLarge    = errors.New("media: file too large")
	ErrUnsupported = errors.New("media: unsupported media type")
	ErrTranscribe  = errors.New("media: transcription failed")
)

// Artifact is one ingested media item. For AUDIO the transcript IS the
// artifact: after a successful transcription the audio file is deleted and
// Path is empty; on transcription failure the file is kept (Path set) so
// nothing is lost.
type Artifact struct {
	ID         string    `json:"id"`
	Path       string    `json:"path"` // absolute path on disk ("" for transcribed audio: file deleted)
	Kind       Kind      `json:"kind"` // image | audio | document
	Mime       string    `json:"mime"`
	Name       string    `json:"name"` // original filename
	Size       int64     `json:"size"` // bytes ingested
	DurationS  int       `json:"duration_s,omitempty"` // audio only
	Transcript string    `json:"transcript,omitempty"` // audio only
	Language   string    `json:"language,omitempty"`   // audio only (whisper-detected)
	CreatedAt  time.Time `json:"created_at"`
}

// Request is one ingest: the raw bytes plus provenance. DurationS is optional
// source-supplied metadata (e.g. Telegram voice.duration); when absent the
// transcriber's measured duration is used for audio.
type Request struct {
	Reader    io.Reader
	Filename  string // original filename (extension drives kind detection with Mime)
	Mime      string
	Source    string // "telegram", "web", ... (becomes the storage subdir)
	ChatLabel string // chat/user label, recorded in the run-log only
	DurationS int    // optional known duration in seconds (audio)
}

// Service owns media storage + processing. Zero-value fields get defaults via
// the accessors; construct with the fields you know.
type Service struct {
	Dir         string        // storage root; files land in Dir/<source>/
	MaxBytes    int64         // per-file cap (default 20 MB)
	Retention   time.Duration // sweeper deletes files older than this (default 168h)
	Transcriber Transcriber   // nil = voice wired but disabled (typed transcribe failure)
	Log         *runlog.Log   // optional run-log for media events
}

func (s *Service) maxBytes() int64 {
	if s.MaxBytes > 0 {
		return s.MaxBytes
	}
	return 20 << 20
}

func (s *Service) retention() time.Duration {
	if s.Retention > 0 {
		return s.Retention
	}
	return 168 * time.Hour
}

// record appends a media event to the run-log (best-effort).
func (s *Service) record(event string, a Artifact, req Request, errMsg string) {
	if s.Log == nil {
		return
	}
	_ = s.Log.Append("media", map[string]any{
		"event": event, "id": a.ID, "source": req.Source, "chat": req.ChatLabel,
		"kind": string(a.Kind), "mime": a.Mime, "name": a.Name, "bytes": a.Size,
		"error": errMsg,
	})
}

func newID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// sanitizeName keeps a filename filesystem- and shell-safe.
func sanitizeName(name string) string {
	name = filepath.Base(name)
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := strings.Trim(b.String(), "._")
	if out == "" {
		out = "file"
	}
	if len(out) > 80 {
		out = out[len(out)-80:]
	}
	return out
}

var imageExts = map[string]bool{".jpg": true, ".jpeg": true, ".png": true, ".gif": true, ".webp": true, ".heic": true, ".bmp": true, ".tiff": true}
var audioExts = map[string]bool{".oga": true, ".ogg": true, ".opus": true, ".mp3": true, ".m4a": true, ".wav": true, ".aiff": true, ".aif": true, ".flac": true, ".aac": true, ".mp4a": true, ".webm": true}

// KindFor classifies by mime first, extension second. Video is explicitly
// unsupported (parked scope); ok=false means the caller should refuse.
func KindFor(filename, mime string) (Kind, bool) {
	m := strings.ToLower(strings.TrimSpace(mime))
	switch {
	case strings.HasPrefix(m, "image/"):
		return KindImage, true
	case strings.HasPrefix(m, "audio/"):
		return KindAudio, true
	case strings.HasPrefix(m, "video/"):
		return "", false
	}
	ext := strings.ToLower(filepath.Ext(filename))
	if imageExts[ext] {
		return KindImage, true
	}
	if audioExts[ext] {
		return KindAudio, true
	}
	if ext == ".mov" || ext == ".mkv" || ext == ".avi" {
		return "", false
	}
	return KindDocument, true
}

// Ingest streams req.Reader to disk under Dir/<source>/, enforcing the size
// cap, classifies the artifact, and for AUDIO transcribes during ingest:
// success deletes the audio file (transcript is the artifact); failure keeps
// the file and returns the artifact alongside a typed ErrTranscribe so the
// caller can both apologize and retain the bytes.
func (s *Service) Ingest(ctx context.Context, req Request) (Artifact, error) {
	kind, ok := KindFor(req.Filename, req.Mime)
	if !ok {
		s.record("failed", Artifact{Mime: req.Mime, Name: req.Filename}, req, "unsupported")
		return Artifact{}, fmt.Errorf("%w: %s (%s)", ErrUnsupported, req.Filename, req.Mime)
	}
	source := req.Source
	if source == "" {
		source = "unknown"
	}
	dir := filepath.Join(s.Dir, source)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Artifact{}, fmt.Errorf("media: mkdir %s: %w", dir, err)
	}
	id := newID()
	name := sanitizeName(req.Filename)
	path := filepath.Join(dir, time.Now().UTC().Format("20060102-150405")+"-"+id+"-"+name)

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return Artifact{}, fmt.Errorf("media: create %s: %w", path, err)
	}
	max := s.maxBytes()
	n, copyErr := io.Copy(f, io.LimitReader(req.Reader, max+1))
	closeErr := f.Close()
	if copyErr != nil || closeErr != nil {
		_ = os.Remove(path)
		if copyErr == nil {
			copyErr = closeErr
		}
		return Artifact{}, fmt.Errorf("media: write %s: %w", path, copyErr)
	}
	if n > max {
		_ = os.Remove(path)
		a := Artifact{ID: id, Kind: kind, Mime: req.Mime, Name: name, Size: n}
		s.record("failed", a, req, "too_large")
		return Artifact{}, fmt.Errorf("%w: over %d MB cap", ErrTooLarge, max>>20)
	}

	art := Artifact{
		ID: id, Path: path, Kind: kind, Mime: req.Mime, Name: name,
		Size: n, DurationS: req.DurationS, CreatedAt: time.Now().UTC(),
	}
	s.record("ingested", art, req, "")

	if kind != KindAudio {
		return art, nil
	}

	// Audio: transcribe during ingest.
	if s.Transcriber == nil {
		s.record("failed", art, req, "transcribe: no transcriber configured (whisper_key unset)")
		return art, fmt.Errorf("%w: no transcriber configured (set [media] whisper_key)", ErrTranscribe)
	}
	tr, terr := s.Transcriber.Transcribe(ctx, path)
	if terr != nil {
		// Keep the file so nothing is lost; the artifact still references it.
		s.record("failed", art, req, "transcribe: "+terr.Error())
		return art, fmt.Errorf("%w: %v", ErrTranscribe, terr)
	}
	art.Transcript = tr.Text
	art.Language = tr.Language
	if art.DurationS == 0 {
		art.DurationS = tr.DurationS
	}
	// Success: the transcript is the artifact; delete the audio file.
	if rmErr := os.Remove(path); rmErr != nil {
		log.Printf("media: could not delete transcribed audio %s: %v", path, rmErr)
	} else {
		art.Path = ""
	}
	s.record("transcribed", art, req, "")
	return art, nil
}

// Sweep deletes files under Dir older than the retention window (by mtime)
// and prunes empty source subdirs. Returns how many files were removed.
func (s *Service) Sweep() int {
	cutoff := time.Now().Add(-s.retention())
	removed := 0
	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		return 0
	}
	for _, e := range entries {
		sub := filepath.Join(s.Dir, e.Name())
		if !e.IsDir() {
			continue
		}
		files, err := os.ReadDir(sub)
		if err != nil {
			continue
		}
		left := 0
		for _, fe := range files {
			p := filepath.Join(sub, fe.Name())
			info, err := fe.Info()
			if err != nil {
				continue
			}
			if info.Mode().IsRegular() && info.ModTime().Before(cutoff) {
				if os.Remove(p) == nil {
					removed++
					continue
				}
			}
			left++
		}
		if left == 0 {
			_ = os.Remove(sub) // best-effort prune; fails harmlessly if non-empty
		}
	}
	if removed > 0 && s.Log != nil {
		_ = s.Log.Append("media", map[string]any{"event": "swept", "removed": removed})
	}
	return removed
}

// StartSweeper runs Sweep hourly until ctx is done (plus once at startup).
func (s *Service) StartSweeper(ctx context.Context) {
	go func() {
		s.Sweep()
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.Sweep()
			}
		}
	}()
}

// EnsureGitignoreLine appends line to root/.gitignore if not already present,
// so workspace autocommit never sweeps media binaries into git history.
func EnsureGitignoreLine(root, line string) error {
	p := filepath.Join(root, ".gitignore")
	data, err := os.ReadFile(p)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, l := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(l) == line {
			return nil
		}
	}
	out := string(data)
	if out != "" && !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	out += line + "\n"
	return os.WriteFile(p, []byte(out), 0o644)
}

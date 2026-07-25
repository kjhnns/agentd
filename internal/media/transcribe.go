// Transcription: a minimal OpenAI Whisper API client, pure stdlib multipart.
// Proven pipeline (clawd's production voice-memo transcriber): model whisper-1,
// response_format verbose_json, auto-detect language unless a hint is
// configured, 300s HTTP timeout. Telegram opus .oga files POST DIRECTLY with
// no ffmpeg re-encode; Whisper accepts ogg/opus, wav, m4a, mp3, aiff natively.
package media

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// Transcript is the result of one transcription.
type Transcript struct {
	Text      string
	Language  string
	DurationS int
}

// Transcriber turns an audio file into a transcript. The media service treats
// it as pluggable so tests stub it and a future local-model swap is trivial.
type Transcriber interface {
	Transcribe(ctx context.Context, path string) (Transcript, error)
}

// Whisper is the OpenAI transcription client.
type Whisper struct {
	APIKey  string
	Model   string // default whisper-1
	Lang    string // optional ISO-639-1 hint; "" = auto-detect
	BaseURL string // default https://api.openai.com/v1 (overridable for tests)
	Client  *http.Client
}

// NewWhisper builds a Whisper client with the production defaults.
func NewWhisper(apiKey, model, lang string) *Whisper {
	if model == "" {
		model = "whisper-1"
	}
	return &Whisper{
		APIKey:  apiKey,
		Model:   model,
		Lang:    lang,
		BaseURL: "https://api.openai.com/v1",
		Client:  &http.Client{Timeout: 300 * time.Second},
	}
}

// Transcribe POSTs the file to /audio/transcriptions and parses the
// verbose_json response ({text, language, duration}).
func (w *Whisper) Transcribe(ctx context.Context, path string) (Transcript, error) {
	if w.APIKey == "" {
		return Transcript{}, fmt.Errorf("whisper: empty API key")
	}
	f, err := os.Open(path)
	if err != nil {
		return Transcript{}, fmt.Errorf("whisper: open %s: %w", path, err)
	}
	defer f.Close()

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile("file", filepath.Base(path))
	if err != nil {
		return Transcript{}, err
	}
	if _, err := io.Copy(part, f); err != nil {
		return Transcript{}, fmt.Errorf("whisper: read %s: %w", path, err)
	}
	_ = mw.WriteField("model", w.Model)
	_ = mw.WriteField("response_format", "verbose_json")
	if w.Lang != "" {
		_ = mw.WriteField("language", w.Lang)
	}
	if err := mw.Close(); err != nil {
		return Transcript{}, err
	}

	base := w.BaseURL
	if base == "" {
		base = "https://api.openai.com/v1"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/audio/transcriptions", &body)
	if err != nil {
		return Transcript{}, err
	}
	req.Header.Set("Authorization", "Bearer "+w.APIKey)
	req.Header.Set("Content-Type", mw.FormDataContentType())

	client := w.Client
	if client == nil {
		client = &http.Client{Timeout: 300 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return Transcript{}, fmt.Errorf("whisper: request: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		snippet := string(respBody)
		if len(snippet) > 300 {
			snippet = snippet[:300]
		}
		return Transcript{}, fmt.Errorf("whisper: HTTP %d: %s", resp.StatusCode, snippet)
	}
	var out struct {
		Text     string  `json:"text"`
		Language string  `json:"language"`
		Duration float64 `json:"duration"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return Transcript{}, fmt.Errorf("whisper: parse response: %w", err)
	}
	return Transcript{Text: out.Text, Language: out.Language, DurationS: int(out.Duration + 0.5)}, nil
}

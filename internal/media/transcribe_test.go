package media

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newWhisperServer fakes the OpenAI transcription endpoint and captures the
// multipart fields of the last request.
func newWhisperServer(t *testing.T, status int, respBody string, got map[string]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/audio/transcriptions" {
			t.Errorf("path = %s, want /audio/transcriptions", r.URL.Path)
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			t.Error("missing bearer auth")
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Fatalf("parse multipart: %v", err)
		}
		for k, v := range r.MultipartForm.Value {
			got[k] = v[0]
		}
		if fhs := r.MultipartForm.File["file"]; len(fhs) == 1 {
			f, _ := fhs[0].Open()
			data, _ := io.ReadAll(f)
			f.Close()
			got["_file_name"] = fhs[0].Filename
			got["_file_bytes"] = string(data)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(respBody))
	}))
}

func writeAudioFixture(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "clip.oga")
	if err := os.WriteFile(p, []byte("OPUSBYTES"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestWhisperTranscribe(t *testing.T) {
	got := map[string]string{}
	srv := newWhisperServer(t, 200,
		`{"text":"the magic word is turquoise","language":"english","duration":3.9}`, got)
	defer srv.Close()

	w := NewWhisper("sk-test", "", "") // model defaults to whisper-1, lang auto
	w.BaseURL = srv.URL

	tr, err := w.Transcribe(context.Background(), writeAudioFixture(t))
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if tr.Text != "the magic word is turquoise" || tr.Language != "english" || tr.DurationS != 4 {
		t.Errorf("transcript = %+v", tr)
	}
	if got["model"] != "whisper-1" {
		t.Errorf("model = %q, want whisper-1", got["model"])
	}
	if got["response_format"] != "verbose_json" {
		t.Errorf("response_format = %q, want verbose_json", got["response_format"])
	}
	if _, has := got["language"]; has {
		t.Error("language field sent despite auto-detect default")
	}
	if got["_file_name"] != "clip.oga" || got["_file_bytes"] != "OPUSBYTES" {
		t.Errorf("file part = %q/%q", got["_file_name"], got["_file_bytes"])
	}
}

func TestWhisperLanguagePassthrough(t *testing.T) {
	got := map[string]string{}
	srv := newWhisperServer(t, 200, `{"text":"ok","language":"german","duration":1.0}`, got)
	defer srv.Close()

	w := NewWhisper("sk-test", "whisper-1", "de")
	w.BaseURL = srv.URL
	if _, err := w.Transcribe(context.Background(), writeAudioFixture(t)); err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if got["language"] != "de" {
		t.Errorf("language = %q, want de", got["language"])
	}
}

func TestWhisperNon200(t *testing.T) {
	got := map[string]string{}
	srv := newWhisperServer(t, 401, `{"error":{"message":"bad key"}}`, got)
	defer srv.Close()

	w := NewWhisper("sk-bad", "", "")
	w.BaseURL = srv.URL
	_, err := w.Transcribe(context.Background(), writeAudioFixture(t))
	if err == nil {
		t.Fatal("expected error on 401")
	}
	if !strings.Contains(err.Error(), "HTTP 401") || !strings.Contains(err.Error(), "bad key") {
		t.Errorf("error should carry status + body snippet: %v", err)
	}
}

func TestWhisperEmptyKey(t *testing.T) {
	w := NewWhisper("", "", "")
	if _, err := w.Transcribe(context.Background(), writeAudioFixture(t)); err == nil {
		t.Fatal("expected error with empty API key")
	}
}

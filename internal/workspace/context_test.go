package workspace

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRotateContext(t *testing.T) {
	for _, line := range []string{"checkpoint 界\n"} {
		t.Run(line, func(t *testing.T) {
			w, err := Init(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			data := bytes.Repeat([]byte(line), 10000)
			if err := os.WriteFile(w.ContextMD(), data, 0o644); err != nil {
				t.Fatal(err)
			}
			before, err := w.ComposeSystemPrompt()
			if err != nil {
				t.Fatal(err)
			}
			if err := w.RotateContext(); err != nil {
				t.Fatal(err)
			}
			after, err := w.ComposeSystemPrompt()
			if err != nil || before != after {
				t.Fatalf("injection changed after rotation: %v", err)
			}
			cut := sectionBoundary(data)
			live, err := os.ReadFile(w.ContextMD())
			if err != nil || !bytes.Equal(live, data[:cut]) {
				t.Fatalf("wrong live prefix: %v", err)
			}
			path := filepath.Join(w.Root, "context-archive", time.Now().Format("2006-01")+".md")
			archive, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(archive, data[cut:]) {
				t.Fatalf("wrong archive tail: %v", err)
			}
			if err := w.RotateContext(); err != nil {
				t.Fatal(err)
			}
			again, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(archive, again) {
				t.Fatalf("second rotation changed archive: %v", err)
			}
			if err := os.WriteFile(w.ContextMD(), data, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := w.RotateContext(); err != nil {
				t.Fatal(err)
			}
			again, err = os.ReadFile(path)
			if err != nil || !bytes.Equal(again, append(archive, data[cut:]...)) {
				t.Fatalf("rotation did not append: %v", err)
			}
		})
	}
}

func TestRotateContextFailurePreservesLiveFile(t *testing.T) {
	w, err := Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	data := bytes.Repeat([]byte("checkpoint\n"), 10000)
	if err := os.WriteFile(w.ContextMD(), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.Root, "context-archive"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := w.RotateContext(); err == nil {
		t.Fatal("rotation succeeded with blocked archive")
	}
	got, err := os.ReadFile(w.ContextMD())
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("live file changed on failure: %v", err)
	}
}

// A file without a line boundary inside the budget is left alone.
func TestRotateContextKeepsAFileWithoutLineBoundary(t *testing.T) {
	w, err := Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	data := bytes.Repeat([]byte("one very long line without a newline"), 10000)
	if err := os.WriteFile(w.ContextMD(), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := w.RotateContext(); err != nil {
		t.Fatal(err)
	}
	live, err := os.ReadFile(w.ContextMD())
	if err != nil || !bytes.Equal(live, data) {
		t.Fatalf("live file changed: %v", err)
	}
	if archives, _ := filepath.Glob(filepath.Join(w.Root, "context-archive", "*.md")); len(archives) != 0 {
		t.Fatalf("unexpected archive: %v", archives)
	}
}

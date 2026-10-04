package fuel

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestValueTitle(t *testing.T) {
	cases := []struct {
		data map[string]any
		want string
	}{
		{map[string]any{"item": " walnuts ", "kcal": 180.0}, "walnuts"},
		{map[string]any{"type": recBP, "systolic_mmhg": 128.0, "diastolic_mmhg": 82.0, "note": "x"}, "128/82 mmHg"},
		{map[string]any{"type": recBP, "voids": "r1"}, "Voided blood pressure"},
		{map[string]any{"type": recSymptom, "category": "chest_pain", "present": true}, "chest pain"},
		{map[string]any{"type": recWaist, "method": "tape", "waist_cm": 92.5}, "92.5 cm waist"},
		{map[string]any{"method": "scale", "weight_kg": 78.4, "fat_pct": 18.0}, "78.4 kg, 18 % fat"},
		{map[string]any{"method": "scale", "weight_kg": json.Number("78.40")}, "78.4 kg"},
		{map[string]any{"method": "dexa"}, "dexa"},
		{map[string]any{}, "Entry"},
	}
	for _, c := range cases {
		if got := valueTitle(c.data); got != c.want {
			t.Errorf("valueTitle(%v) = %q, want %q", c.data, got, c.want)
		}
	}
}

func TestWithTitleKeepsACallerTitleAndTheInput(t *testing.T) {
	in := map[string]any{"item": "walnuts"}
	out := withTitle(in)
	if out["title"] != "walnuts" || len(in) != 1 {
		t.Fatalf("out %v, in %v", out, in)
	}
	own := map[string]any{"item": "walnuts", "title": "Snack"}
	if withTitle(own)["title"] != "Snack" {
		t.Fatal("a caller title must be kept")
	}
	if withTitle(map[string]any{"item": "x", "title": " "})["title"] != "x" {
		t.Fatal("a blank title must be replaced")
	}
}

// Every value Post sends carries a title: Variables rejects one without it.
func TestPostSendsATitle(t *testing.T) {
	var sent string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body struct {
			Data string `json:"data"`
		}
		_ = json.Unmarshal(b, &body)
		sent = body.Data
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"v1"}`))
	}))
	defer srv.Close()
	v := &VariablesHTTP{Base: srv.URL, Key: "k", Client: srv.Client()}
	if _, err := v.Post(context.Background(), "var", map[string]any{"item": "walnuts", "kcal": 180}, "2026-10-04"); err != nil {
		t.Fatal(err)
	}
	var d map[string]any
	if err := json.Unmarshal([]byte(sent), &d); err != nil || d["title"] != "walnuts" || d["item"] != "walnuts" {
		t.Fatalf("sent %s", sent)
	}
}

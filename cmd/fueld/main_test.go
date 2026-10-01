package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandlerServesOnlyFuelAndRecoversPanics(t *testing.T) {
	h := Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/fuel/boom" {
			panic("boom")
		}
		w.WriteHeader(204)
	}))
	for path, want := range map[string]int{"/fuel/snapshot": 204, "/sessions": 404, "/": 404, "/watch/ping": 404, "/fuel/boom": 500, "/fuel": 404, "/fuelx": 404} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != want {
			t.Fatalf("%s: %d want %d", path, rec.Code, want)
		}
		if want >= 400 && !strings.Contains(rec.Body.String(), `"error"`) {
			t.Fatalf("%s: body %s", path, rec.Body)
		}
	}
	// The server keeps serving after a panic.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/fuel/x", nil))
	if rec.Code != 204 {
		t.Fatal("not serving after a panic")
	}
}

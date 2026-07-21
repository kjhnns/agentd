// Package api is agentd's local HTTP + WebSocket surface (design 3.6). It binds
// 127.0.0.1 by default and is bearer-gated. Crucially, GET /health returns BOTH
// transport_ok AND per-session agent_ok as DISTINCT signals, the lesson from
// failure class 2 (a green transport health said nothing about a dead agent).
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/kjhnns/agentd/internal/eventbus"
	"github.com/kjhnns/agentd/internal/session"
)

// TransportChecker reports whether the inbound transport (e.g. the Telegram
// long-poll) is currently healthy. It is deliberately separate from agent health.
type TransportChecker func() bool

// Server is the HTTP API server.
type Server struct {
	bearer    string
	mgr       *session.Manager
	bus       *eventbus.Bus
	transport TransportChecker
	mux       *http.ServeMux
}

// New builds the API server. transport may be nil (reported as unknown/false).
func New(bearer string, mgr *session.Manager, bus *eventbus.Bus, transport TransportChecker) *Server {
	s := &Server{bearer: bearer, mgr: mgr, bus: bus, transport: transport, mux: http.NewServeMux()}
	s.routes()
	return s
}

// Handler returns the bearer-gated http.Handler.
func (s *Server) Handler() http.Handler {
	return s.auth(s.mux)
}

func (s *Server) routes() {
	s.mux.HandleFunc("/health", s.handleHealth)
	s.mux.HandleFunc("/sessions", s.handleSessions)        // GET list, POST create
	s.mux.HandleFunc("/sessions/", s.handleSessionSubpath) // /:id, /:id/input, /:id/events, /:id/interrupt
}

// auth enforces the API bearer token (design 3.9). /health is also gated; a bare
// liveness probe can be added unauthenticated later if needed.
func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.bearer != "" {
			got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if got != s.bearer {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// handleHealth returns BOTH transport_ok and per-session agent_ok (design 3.6).
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	transportOK := false
	if s.transport != nil {
		transportOK = s.transport()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":           true,
		"transport_ok": transportOK,
		"sessions":     s.mgr.List(), // each element carries agent_ok
		"ts":           time.Now().UTC(),
	})
}

func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, s.mgr.List())
	case http.MethodPost:
		var body struct {
			Cwd   string `json:"cwd"`
			Model string `json:"model"`
			Title string `json:"title"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		sess, err := s.mgr.Create(r.Context(), body.Cwd, body.Model, body.Title)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]string{"id": sess.ID, "harness": sess.Harness})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleSessionSubpath dispatches /sessions/:id[, /input | /events | /interrupt].
func (s *Server) handleSessionSubpath(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/sessions/")
	parts := strings.SplitN(rest, "/", 2)
	id := parts[0]
	if id == "" {
		http.Error(w, "missing session id", http.StatusBadRequest)
		return
	}
	sub := ""
	if len(parts) == 2 {
		sub = parts[1]
	}
	switch sub {
	case "":
		if r.Method == http.MethodDelete {
			if err := s.mgr.Teardown(id); err != nil {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			writeJSON(w, http.StatusOK, map[string]string{"status": "torn_down", "id": id})
			return
		}
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	case "input":
		s.handleInput(w, r, id)
	case "interrupt":
		if err := s.mgr.Interrupt(id); err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "interrupted", "id": id})
	case "events":
		s.handleEvents(w, r, id)
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

func (s *Server) handleInput(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Text string `json:"text"`
		Key  string `json:"key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	if _, err := s.mgr.Send(r.Context(), id, body.Text); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "sent", "id": id})
}

// handleEvents upgrades to WebSocket and streams normalized events for a session.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request, id string) {
	if _, ok := s.mgr.Get(id); !ok {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	ws, err := wsUpgrade(w, r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	defer ws.Close()

	subID, events := s.bus.Subscribe()
	defer s.bus.Unsubscribe(subID)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			return
		case e, ok := <-events:
			if !ok {
				return
			}
			if e.SessionID != id {
				continue
			}
			payload, _ := json.Marshal(e)
			if err := ws.WriteText(payload); err != nil {
				return
			}
		}
	}
}

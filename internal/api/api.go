// Package api is agentd's local HTTP + WebSocket surface (design 3.6). It binds
// 127.0.0.1 by default and is bearer-gated. Crucially, GET /health returns BOTH
// transport_ok AND per-session agent_ok as DISTINCT signals, the lesson from
// failure class 2 (a green transport health said nothing about a dead agent).
package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/kjhnns/agentd/internal/channel"
	"github.com/kjhnns/agentd/internal/eventbus"
	"github.com/kjhnns/agentd/internal/media"
	"github.com/kjhnns/agentd/internal/scheduler"
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
	sched     *scheduler.Scheduler
	media     *media.Service
	mux       *http.ServeMux
	pub       *http.ServeMux // routes that carry their OWN auth (see MountPublic)
}

// New builds the API server. transport may be nil (reported as unknown/false).
func New(bearer string, mgr *session.Manager, bus *eventbus.Bus, transport TransportChecker) *Server {
	s := &Server{bearer: bearer, mgr: mgr, bus: bus, transport: transport, mux: http.NewServeMux(), pub: http.NewServeMux()}
	s.routes()
	return s
}

// Handler returns the bearer-gated http.Handler. Public routes (MountPublic)
// are matched first and bypass the API bearer: they carry their own gate.
func (s *Server) Handler() http.Handler {
	authed := s.auth(s.mux)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, pattern := s.pub.Handler(r); pattern != "" {
			s.pub.ServeHTTP(w, r)
			return
		}
		authed.ServeHTTP(w, r)
	})
}

// PublicHandler serves ONLY the public routes and 404s everything else. It is
// what an extra listener beyond loopback ([server] public_bind) gets, so a
// proxy misconfiguration can never expose the code-executing API.
func (s *Server) PublicHandler() http.Handler { return s.pub }

func (s *Server) routes() {
	s.mux.HandleFunc("/health", s.handleHealth)
	s.mux.HandleFunc("/sessions", s.handleSessions)        // GET list, POST create
	s.mux.HandleFunc("/sessions/", s.handleSessionSubpath) // /past, /:id, /:id/input, /:id/media, /:id/events, /:id/history, /:id/continue, /:id/interrupt
	s.mux.HandleFunc("/jobs", s.handleJobs)                // GET list
	s.mux.HandleFunc("/jobs/", s.handleJobSubpath)         // /:name/run (POST), /:name/runs (GET)
	s.mux.HandleFunc("/hooks/", s.handleHook)              // POST /hooks/:job (webhook trigger)
}

// AttachScheduler wires the jobs/scheduler surface (GET /jobs, POST
// /jobs/:name/run, GET /jobs/:name/runs, POST /hooks/:job). Without it those
// routes answer 503. All routes share the API bearer gate.
func (s *Server) AttachScheduler(sched *scheduler.Scheduler) { s.sched = sched }

// AttachMedia wires the core media service so POST /sessions/:id/media works.
// Without it (or with nil) the route answers 503.
func (s *Server) AttachMedia(svc *media.Service) { s.media = svc }

// Mount registers an extra handler on the shared mux so a channel (e.g. the web
// UI) is served by the ONE HTTP server behind the SAME bearer gate, on no second
// port. Patterns follow http.ServeMux rules (a trailing slash matches a subtree).
func (s *Server) Mount(pattern string, h http.Handler) { s.mux.Handle(pattern, h) }

// MountPublic registers a handler that is reachable WITHOUT the API bearer, on
// the main listener and on the public one. The handler MUST enforce its own
// authentication (the watch channel gates on its own token). Patterns follow
// http.ServeMux rules.
func (s *Server) MountPublic(pattern string, h http.Handler) { s.pub.Handle(pattern, h) }

// authed reports whether a request carries the API bearer. The header form
// (Authorization: Bearer ...) is the canonical machine path. For BROWSERS, which
// cannot set a header on navigation or a WebSocket handshake, the SAME bearer is
// also accepted as a ?token= query param (used on first UI load) or an
// agentd_token cookie (which the /ui handler sets from that query param). This is
// still the single bearer secret; it is bound to 127.0.0.1 by default.
func (s *Server) authed(r *http.Request) bool {
	if s.bearer == "" {
		return true
	}
	if strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ") == s.bearer {
		return true
	}
	if r.URL.Query().Get("token") == s.bearer {
		return true
	}
	if c, err := r.Cookie("agentd_token"); err == nil && c.Value == s.bearer {
		return true
	}
	return false
}

// auth enforces the API bearer token (design 3.9). /health is also gated; a bare
// liveness probe can be added unauthenticated later if needed.
func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.authed(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
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
			Workspace string `json:"workspace"` // workspace name ("" = default)
			Cwd       string `json:"cwd"`       // honored only without a workspace store
			Model     string `json:"model"`
			Title     string `json:"title"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		sess, err := s.mgr.Create(r.Context(), body.Workspace, body.Cwd, body.Model, body.Title)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]string{"id": sess.ID, "harness": sess.Harness, "workspace": sess.Workspace})
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
	if id == "past" && sub == "" {
		s.handlePastSessions(w, r)
		return
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
	case "reset":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			Reason string `json:"reason"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		reason := body.Reason
		if reason == "" {
			reason = "explicit (api)"
		}
		if err := s.mgr.Reset(r.Context(), id, reason); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "reset", "id": id, "reason": reason})
	case "events":
		s.handleEvents(w, r, id)
	case "history":
		s.handleHistory(w, r, id)
	case "continue":
		s.handleContinue(w, r, id)
	case "media":
		s.handleMediaUpload(w, r, id)
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

// handleMediaUpload accepts a multipart upload (field "file", optional field
// "text") for a LIVE session: POST /sessions/:id/media. The bytes go through
// the SAME core media.Ingest as any channel, then the artifact is rendered by
// the session layer (session.RenderInbound) and routed as one ordinary turn
// into the session. The response returns after the turn completes (mirroring
// /input); the turn's events also stream over the WS as usual.
func (s *Server) handleMediaUpload(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.media == nil {
		http.Error(w, "media not configured", http.StatusServiceUnavailable)
		return
	}
	if _, ok := s.mgr.Get(id); !ok {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	// Bound the body BEFORE multipart parsing (which spools to temp files); the
	// media service re-checks its own cap on the bytes it actually ingests.
	cap := int64(20 << 20)
	if s.media.MaxBytes > 0 {
		cap = s.media.MaxBytes
	}
	r.Body = http.MaxBytesReader(w, r.Body, cap+(1<<20))
	file, hdr, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "missing multipart field \"file\"", http.StatusBadRequest)
		return
	}
	defer file.Close()

	art, err := s.media.Ingest(r.Context(), media.Request{
		Reader:    file,
		Filename:  hdr.Filename,
		Mime:      hdr.Header.Get("Content-Type"),
		Source:    "web",
		ChatLabel: id,
	})
	if err != nil {
		switch {
		case errors.Is(err, media.ErrTooLarge):
			http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
		case errors.Is(err, media.ErrUnsupported):
			http.Error(w, err.Error(), http.StatusUnsupportedMediaType)
		case errors.Is(err, media.ErrTranscribe):
			http.Error(w, err.Error(), http.StatusBadGateway)
		default:
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}

	text := s.mgr.RenderTurn(channel.InboundMsg{
		Channel: "web",
		Text:    r.FormValue("text"),
		Media:   []media.Artifact{art},
	})
	result, err := s.mgr.SendAndCollect(r.Context(), id, text, session.TurnOptions{
		Timeout: s.mgr.TurnTimeout(),
		Ceiling: s.mgr.TurnCeiling(),
	})
	if err != nil {
		code := http.StatusInternalServerError
		if errors.Is(err, session.ErrTurnTimeout) {
			code = http.StatusGatewayTimeout
		}
		http.Error(w, err.Error(), code)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "routed", "id": id, "artifact": art, "result": result})
}

// handlePastSessions lists sessions derivable from the run-log that are no
// longer live (idle-GC reclaimed or pre-restart), most recent first, capped at
// session.PastSessionsMaxList (?limit=N narrows): GET /sessions/past.
func (s *Server) handlePastSessions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	limit := 0
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}
	list, err := s.mgr.PastSessions(limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// handleContinue starts a NEW live session seeded with a compact rendering of
// a past session's conversation (POST /sessions/:id/continue). This is
// continue-as-new-session: the source id is not resurrected (attach-survives-
// restart is a reserved future design); the new session is titled
// "continued:<source id>" and its first turn (running in the background when
// this returns) delivers the transcript to the model.
func (s *Server) handleContinue(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	sess, err := s.mgr.Continue(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{
		"id": sess.ID, "source": id, "title": sess.Title, "status": "continuing",
	})
}

// handleHistory replays a session's prior conversation from the durable
// run-log (GET /sessions/:id/history[?turns=N], default/cap
// session.HistoryMaxTurns) so the UI can backfill after a page reload. A
// session with nothing logged yet yields an empty list, not a 404.
func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	turns := 0
	if v := r.URL.Query().Get("turns"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n < session.HistoryMaxTurns {
			turns = n
		}
	}
	entries, err := s.mgr.History(id, turns)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, entries)
}

// handleJobs lists jobs with next-fire + last-run status.
func (s *Server) handleJobs(w http.ResponseWriter, r *http.Request) {
	if s.sched == nil {
		http.Error(w, "scheduler not configured", http.StatusServiceUnavailable)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, s.sched.List())
}

// handleJobSubpath dispatches /jobs/:name/run (POST, manual fire-now) and
// /jobs/:name/runs (GET, recent run history).
func (s *Server) handleJobSubpath(w http.ResponseWriter, r *http.Request) {
	if s.sched == nil {
		http.Error(w, "scheduler not configured", http.StatusServiceUnavailable)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/jobs/")
	parts := strings.SplitN(rest, "/", 2)
	name := parts[0]
	sub := ""
	if len(parts) == 2 {
		sub = parts[1]
	}
	if name == "" {
		http.Error(w, "missing job name", http.StatusBadRequest)
		return
	}
	switch sub {
	case "run":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		queued, reason, err := s.sched.RunNow(name, "manual")
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		if !queued {
			writeJSON(w, http.StatusConflict, map[string]string{"status": "skipped", "job": name, "reason": reason})
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "queued", "job": name})
	case "runs":
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		runs, err := s.sched.Runs(name, 20)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, runs)
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

// handleHook fires a webhook-trigger job: POST /hooks/:job. It is gated by the
// same API bearer as everything else and only fires jobs whose trigger kind is
// "webhook" (schedule/file jobs are fired manually via /jobs/:name/run).
func (s *Server) handleHook(w http.ResponseWriter, r *http.Request) {
	if s.sched == nil {
		http.Error(w, "scheduler not configured", http.StatusServiceUnavailable)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/hooks/")
	job, ok := s.sched.Get(name)
	if !ok {
		http.Error(w, "unknown job", http.StatusNotFound)
		return
	}
	if job.Trigger.Kind != scheduler.TriggerWebhook {
		http.Error(w, "job is not webhook-triggered", http.StatusNotFound)
		return
	}
	if !job.Enabled {
		http.Error(w, "job disabled", http.StatusConflict)
		return
	}
	queued, reason, err := s.sched.RunNow(name, "webhook")
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if !queued {
		writeJSON(w, http.StatusConflict, map[string]string{"status": "skipped", "job": name, "reason": reason})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "queued", "job": name})
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
	if _, ok := s.mgr.Get(id); !ok {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	// Return the turn's ANSWER, not just an ack: a non-streaming client (the
	// `agentd run` CLI, a script with curl) has no other way to read it, and the
	// call already blocked for the whole turn. Streaming clients (the web UI)
	// keep reading the same reply off the WS and ignore this field.
	result, err := s.mgr.SendAndCollect(r.Context(), id, body.Text, session.TurnOptions{
		Timeout: s.mgr.TurnTimeout(),
		Ceiling: s.mgr.TurnCeiling(),
	})
	if err != nil {
		code := http.StatusInternalServerError
		if errors.Is(err, session.ErrTurnTimeout) {
			code = http.StatusGatewayTimeout
		}
		http.Error(w, err.Error(), code)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "sent", "id": id, "result": result})
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

package fuel

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// The Fuel web app (spec section 23): a browser session next to the bearer
// path of the phone, and the static files of the page, embedded.
//
// Rules of this file:
//   - Only the SHA-256 of a session id is stored. The cookie is HttpOnly.
//   - A cookie request that is not GET or HEAD needs Origin = public_origin
//     and the X-Fuel-CSRF header of its session.
//   - A request with an Authorization header is judged by the bearer alone.
//   - Nothing here logs a token, a session id, a cookie or a CSRF token.

//go:embed web
var webFiles embed.FS

// WebOptions switches the web app on (config web_enabled, public_origin).
type WebOptions struct {
	Enabled      bool
	PublicOrigin string // "https://fuel.gojoe.run": the one origin the page is served on
	// InsecureTestCookie drops Secure and the __Host- prefix of the cookie so
	// a browser test can run on a plain http test instance. Refused unless
	// test_mode is on and public_origin is http.
	InsecureTestCookie bool
}

const (
	webCookieName     = "__Host-fuel_session"
	webCookieNameTest = "fuel_session_test"
	webCSRFHeader     = "X-Fuel-CSRF"
	webSessionSlide   = 30 * 24 * time.Hour
	webSessionCap     = 90 * 24 * time.Hour
	webRenewAfter     = time.Hour
	webMaxSessions    = 5
	webLoginBody      = 4 << 10
	webLoginRead      = 5 * time.Second
	webLoginInflight  = 4
	webLoginFailLimit = 5
	webCSP            = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' blob:; " +
		"connect-src 'self'; manifest-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'"
)

// Validate checks the web options against the rest of the config.
func (w WebOptions) validate(testMode bool) error {
	if !w.Enabled {
		if w.InsecureTestCookie {
			return errors.New("fuel: web_insecure_test_cookie needs web_enabled")
		}
		return nil
	}
	u, err := url.Parse(w.PublicOrigin)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") ||
		u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return errors.New(`fuel: web_enabled needs public_origin as scheme://host[:port] with no path ("https://fuel.gojoe.run")`)
	}
	if w.InsecureTestCookie {
		if u.Scheme == "https" {
			return errors.New("fuel: web_insecure_test_cookie is refused with an https public_origin")
		}
		if !testMode {
			return errors.New("fuel: web_insecure_test_cookie is refused without test_mode")
		}
		return nil
	}
	if u.Scheme != "https" {
		return errors.New("fuel: public_origin must be https (the session cookie is Secure)")
	}
	return nil
}

type webSession struct {
	Hash     string    `json:"hash"` // hex SHA-256 of the session id
	Created  time.Time `json:"created_at"`
	LastSeen time.Time `json:"last_seen"`
	Expires  time.Time `json:"expires_at"`
	Agent    string    `json:"user_agent"`
}

type webFile struct {
	body  []byte
	ctype string
	etag  string
}

// webState is the session store plus the static files.
type webState struct {
	o      WebOptions
	now    func() time.Time
	path   string
	key    []byte // HMAC key of the CSRF tokens (state_dir/web.key, 0600)
	tag    string // which app token the sessions were issued under
	files  map[string]webFile
	logins chan struct{}
	fails  authFailures

	mu       sync.Mutex
	sessions []webSession
}

type webStoreFile struct {
	TokenTag string       `json:"token_tag"`
	Sessions []webSession `json:"sessions"`
}

func openWeb(o WebOptions, stateDir, token string, now func() time.Time) (*webState, error) {
	ws := &webState{o: o, now: now, path: filepath.Join(stateDir, "web-sessions.json"),
		files: map[string]webFile{}, logins: make(chan struct{}, webLoginInflight)}
	keyPath := filepath.Join(stateDir, "web.key")
	key, err := os.ReadFile(keyPath)
	if err != nil || len(key) != 32 {
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		key = make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		if err := writeFileAtomic(keyPath, key); err != nil {
			return nil, err
		}
	}
	ws.key = key
	ws.tag = ws.mac("token:" + token)

	// Sessions survive a restart. A changed app token or an unreadable file
	// drops all of them.
	if b, err := os.ReadFile(ws.path); err == nil {
		var f webStoreFile
		if json.Unmarshal(b, &f) == nil && hmac.Equal([]byte(f.TokenTag), []byte(ws.tag)) {
			t := now()
			for _, s := range f.Sessions {
				if t.Before(s.Expires) && len(s.Hash) == 64 {
					ws.sessions = append(ws.sessions, s)
				}
			}
		}
		if err := ws.saveLocked(); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	// The static files: a fixed allow list built from the embed. A request
	// path is only ever a key of this map, never joined to a file path.
	err = fs.WalkDir(webFiles, "web", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := webFiles.ReadFile(p)
		if err != nil {
			return err
		}
		ct := mime.TypeByExtension(filepath.Ext(p))
		switch filepath.Ext(p) {
		case ".js":
			ct = "text/javascript; charset=utf-8"
		case ".css":
			ct = "text/css; charset=utf-8"
		case ".html":
			ct = "text/html; charset=utf-8"
		case ".svg":
			ct = "image/svg+xml"
		}
		if ct == "" {
			ct = "application/octet-stream"
		}
		sum := sha256.Sum256(b)
		ws.files[strings.TrimPrefix(p, "web/")] = webFile{body: b, ctype: ct, etag: `"` + hex.EncodeToString(sum[:8]) + `"`}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if _, ok := ws.files["index.html"]; !ok {
		return nil, errors.New("fuel: the embedded web app has no index.html")
	}
	return ws, nil
}

func writeFileAtomic(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (ws *webState) mac(v string) string {
	m := hmac.New(sha256.New, ws.key)
	m.Write([]byte(v))
	return hex.EncodeToString(m.Sum(nil))
}

func webHash(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:])
}

// csrf is the CSRF token of a session: an HMAC of its id, so the store
// holds nothing that a thief of the file could send.
func (ws *webState) csrf(id string) string { return ws.mac("csrf:" + id) }

func (ws *webState) saveLocked() error {
	if ws.sessions == nil {
		ws.sessions = []webSession{}
	}
	b, err := json.Marshal(webStoreFile{TokenTag: ws.tag, Sessions: ws.sessions})
	if err != nil {
		return err
	}
	return writeFileAtomic(ws.path, b)
}

func (ws *webState) cookieName() string {
	if ws.o.InsecureTestCookie {
		return webCookieNameTest
	}
	return webCookieName
}

func (ws *webState) setCookie(w http.ResponseWriter, id string, maxAge int) {
	c := &http.Cookie{Name: ws.cookieName(), Value: id, Path: "/", MaxAge: maxAge,
		HttpOnly: true, Secure: !ws.o.InsecureTestCookie, SameSite: http.SameSiteStrictMode}
	w.Header().Add("Set-Cookie", c.String())
}

func (ws *webState) clearCookie(w http.ResponseWriter) { ws.setCookie(w, "", -1) }

// cookieID returns the session id of the request's cookie ("" when absent).
func (ws *webState) cookieID(r *http.Request) string {
	c, err := r.Cookie(ws.cookieName())
	if err != nil || len(c.Value) < 20 || len(c.Value) > 100 {
		return ""
	}
	return c.Value
}

// create makes a session and returns its id. Over the limit, the oldest goes.
func (ws *webState) create(agent string) (string, time.Time, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", time.Time{}, err
	}
	id := base64.RawURLEncoding.EncodeToString(raw)
	now := ws.now()
	if len(agent) > 120 {
		agent = agent[:120]
	}
	ws.mu.Lock()
	defer ws.mu.Unlock()
	prev := append([]webSession(nil), ws.sessions...)
	live := ws.sessions[:0]
	for _, s := range ws.sessions {
		if now.Before(s.Expires) {
			live = append(live, s)
		}
	}
	sort.SliceStable(live, func(i, j int) bool { return live[i].Created.Before(live[j].Created) })
	for len(live) >= webMaxSessions {
		live = live[1:]
	}
	exp := now.Add(webSessionSlide)
	ws.sessions = append(live, webSession{Hash: webHash(id), Created: now, LastSeen: now, Expires: exp, Agent: agent})
	if err := ws.saveLocked(); err != nil {
		ws.sessions = prev
		return "", time.Time{}, err
	}
	return id, exp, nil
}

// lookup finds the live session of an id. renewed is true when the session
// slid forward (the caller sends the cookie again).
func (ws *webState) lookup(id string) (sess webSession, ok, renewed bool) {
	if id == "" {
		return webSession{}, false, false
	}
	h := webHash(id)
	now := ws.now()
	ws.mu.Lock()
	defer ws.mu.Unlock()
	for i := range ws.sessions {
		s := &ws.sessions[i]
		if subtle.ConstantTimeCompare([]byte(s.Hash), []byte(h)) != 1 {
			continue
		}
		if !now.Before(s.Expires) {
			ws.sessions = append(ws.sessions[:i], ws.sessions[i+1:]...)
			_ = ws.saveLocked()
			return webSession{}, false, false
		}
		if now.Sub(s.LastSeen) > webRenewAfter {
			s.LastSeen = now
			s.Expires = now.Add(webSessionSlide)
			if hard := s.Created.Add(webSessionCap); s.Expires.After(hard) {
				s.Expires = hard
			}
			_ = ws.saveLocked() // a failed save keeps the session in memory
			renewed = true
		}
		return *s, true, renewed
	}
	return webSession{}, false, false
}

// drop ends one session (all = every session).
func (ws *webState) drop(id string, all bool) error {
	h := webHash(id)
	ws.mu.Lock()
	defer ws.mu.Unlock()
	keep := ws.sessions[:0]
	if !all {
		for _, s := range ws.sessions {
			if s.Hash != h {
				keep = append(keep, s)
			}
		}
	}
	ws.sessions = keep
	return ws.saveLocked()
}

// csrfOK is the rule for a cookie request. A read must not come from
// another origin's page; a write needs the Origin and the header token.
func (ws *webState) csrfOK(r *http.Request, id string) bool {
	// Browsers name the relation of the page that made the request. A
	// sibling host (same site) or another site never reads with the cookie.
	if sf := r.Header.Get("Sec-Fetch-Site"); sf != "" && sf != "same-origin" && sf != "none" {
		return false
	}
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return true
	}
	if r.Header.Get("Origin") != ws.o.PublicOrigin {
		return false
	}
	got := r.Header.Get(webCSRFHeader)
	return got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(ws.csrf(id))) == 1
}

// cookieAuth authenticates a request by its session cookie. It is called
// only for a request WITHOUT an Authorization header.
func (ws *webState) cookieAuth(w http.ResponseWriter, r *http.Request) (ok bool, e *apiError) {
	id := ws.cookieID(r)
	sess, found, renewed := ws.lookup(id)
	if !found {
		return false, nil
	}
	if !ws.csrfOK(r, id) {
		return false, errf(http.StatusForbidden, "csrf", false, "cross-origin or unsigned request refused")
	}
	if renewed {
		ws.setCookie(w, id, int(sess.Expires.Sub(ws.now()).Seconds()))
	}
	return true, nil
}

// headers go on every answer while the web app is on.
func (ws *webState) headers(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Security-Policy", webCSP)
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
	h.Set("Cross-Origin-Opener-Policy", "same-origin")
	h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
	if strings.HasPrefix(ws.o.PublicOrigin, "https://") {
		h.Set("Strict-Transport-Security", "max-age=31536000")
	}
}

func isWebPath(p string) bool {
	return p == "/fuel/session" || p == "/fuel/web" || strings.HasPrefix(p, "/fuel/web/")
}

// serveWeb answers the routes that work without a session: the static files
// and the session routes. They never wait for readiness or the targets file.
func (s *Service) serveWeb(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	ws := s.web
	if ws == nil {
		writeErr(w, errf(http.StatusNotFound, "not_found", false, "no such route"))
		return
	}
	ws.headers(w)
	if r.URL.Path == "/fuel/session" {
		switch r.Method {
		case http.MethodPost:
			s.webLogin(w, r)
		case http.MethodGet:
			s.webSessionGet(w, r)
		case http.MethodDelete:
			s.webLogout(w, r)
		default:
			w.Header().Set("Allow", "GET, POST, DELETE")
			writeErr(w, errf(http.StatusMethodNotAllowed, "bad_input", false, "method not allowed"))
		}
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeErr(w, errf(http.StatusMethodNotAllowed, "bad_input", false, "method not allowed"))
		return
	}
	if r.URL.Path == "/fuel/web" {
		w.Header().Set("Location", "/fuel/web/")
		w.WriteHeader(http.StatusPermanentRedirect)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/fuel/web/")
	if name == "" {
		name = "index.html"
	}
	f, ok := ws.files[name]
	if !ok {
		writeErr(w, errf(http.StatusNotFound, "not_found", false, "no such file"))
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("ETag", f.etag)
	if r.Header.Get("If-None-Match") == f.etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", f.ctype)
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = w.Write(f.body)
	}
}

// webLogin is POST /fuel/session. Every limit is checked before the body is
// parsed: Origin, Content-Type, requests in flight, size, read deadline.
func (s *Service) webLogin(w http.ResponseWriter, r *http.Request) {
	ws := s.web
	if r.Header.Get("Origin") != ws.o.PublicOrigin {
		writeErr(w, errf(http.StatusBadRequest, "wrong_origin", false, "sign in on the Fuel page"))
		return
	}
	if ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); ct != "application/json" {
		writeErr(w, errf(http.StatusUnsupportedMediaType, "unsupported_media", false, "application/json only"))
		return
	}
	select {
	case ws.logins <- struct{}{}:
		defer func() { <-ws.logins }()
	default:
		e := errf(http.StatusServiceUnavailable, "busy", true, "too many sign-in requests")
		e.retryAfter = 2
		writeErr(w, e)
		return
	}
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(webLoginRead))
	r.Body = http.MaxBytesReader(w, r.Body, webLoginBody)
	var body struct {
		Token *string `json:"token"`
	}
	err := decodeStrict(r.Body, &body)
	_ = rc.SetReadDeadline(time.Time{})
	if err != nil {
		if maxBytesErr(err) {
			writeErr(w, errf(http.StatusRequestEntityTooLarge, "too_large", false, "body too large"))
			return
		}
		writeErr(w, errf(http.StatusBadRequest, "bad_input", false, "bad JSON body"))
		return
	}
	if body.Token == nil {
		writeErr(w, errf(http.StatusBadRequest, "bad_input", false, "token is required"))
		return
	}
	// A correct token is always accepted, so the brake cannot lock Joe out.
	// Only the app token signs in; the agent token never does.
	if s.o.Token == "" || subtle.ConstantTimeCompare([]byte(*body.Token), []byte(s.o.Token)) != 1 {
		ws.fails.mu.Lock()
		now := s.o.Now()
		if now.Sub(ws.fails.window) > authFailWindow {
			ws.fails.window, ws.fails.count = now, 0
		}
		ws.fails.count++
		n := ws.fails.count
		ra := int((authFailWindow - now.Sub(ws.fails.window)).Seconds()) + 1
		ws.fails.mu.Unlock()
		// Sampled: the first failure of a window and the moment the brake engages.
		if n == 1 {
			log.Printf("fuel: web sign-in refused from %s", clientIP(r))
		}
		if n > webLoginFailLimit {
			if n == webLoginFailLimit+1 {
				log.Printf("fuel: web sign-in brake engaged, last from %s", clientIP(r))
			}
			e := errf(http.StatusTooManyRequests, "rate_limited", true, "too many failed attempts")
			e.retryAfter = ra
			writeErr(w, e)
			return
		}
		writeErr(w, errf(http.StatusUnauthorized, "unauthorized", false, "wrong token"))
		return
	}
	id, exp, err := ws.create(r.UserAgent())
	if err != nil {
		log.Printf("fuel: web session not stored: %v", err)
		writeErr(w, errf(http.StatusInternalServerError, "internal", true, "the session could not be stored"))
		return
	}
	ws.setCookie(w, id, int(exp.Sub(s.o.Now()).Seconds()))
	w.WriteHeader(http.StatusNoContent)
}

// webSessionGet is GET /fuel/session: the CSRF token of a live session.
func (s *Service) webSessionGet(w http.ResponseWriter, r *http.Request) {
	ws := s.web
	id := ws.cookieID(r)
	sess, ok, renewed := ws.lookup(id)
	if !ok || r.Header.Get("Authorization") != "" {
		writeErr(w, errf(http.StatusUnauthorized, "unauthorized", false, "no session"))
		return
	}
	if !ws.csrfOK(r, id) {
		writeErr(w, errf(http.StatusForbidden, "csrf", false, "cross-origin request refused"))
		return
	}
	if renewed {
		ws.setCookie(w, id, int(sess.Expires.Sub(s.o.Now()).Seconds()))
	}
	out := map[string]any{"csrf": ws.csrf(id), "expires_at": sess.Expires.UTC().Format(time.RFC3339), "test_mode": s.o.TestMode}
	if s.o.TestMode {
		out["food_log_var"] = s.o.FoodVar
	}
	writeJSON(w, http.StatusOK, out)
}

// webLogout is DELETE /fuel/session (?all=1 ends every session).
func (s *Service) webLogout(w http.ResponseWriter, r *http.Request) {
	ws := s.web
	id := ws.cookieID(r)
	if _, ok, _ := ws.lookup(id); ok {
		if !ws.csrfOK(r, id) {
			writeErr(w, errf(http.StatusForbidden, "csrf", false, "cross-origin or unsigned request refused"))
			return
		}
		if err := ws.drop(id, r.URL.Query().Get("all") == "1"); err != nil {
			log.Printf("fuel: web session not removed: %v", err)
			writeErr(w, errf(http.StatusInternalServerError, "internal", true, "the session could not be removed"))
			return
		}
	}
	ws.clearCookie(w)
	w.WriteHeader(http.StatusNoContent)
}

func (ws *webState) String() string { return fmt.Sprintf("web(%d files)", len(ws.files)) }

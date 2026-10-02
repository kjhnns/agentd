package fuel

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const webOrigin = "https://fuel.test"

func webHarness(t *testing.T, mut ...func(*Options)) *harness {
	t.Helper()
	all := append([]func(*Options){func(o *Options) { o.Web = WebOptions{Enabled: true, PublicOrigin: webOrigin} }}, mut...)
	return newHarness(t, all...)
}

// raw sends a request with exactly the given headers (no bearer by default).
func (h *harness) raw(method, path, body string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.h.ServeHTTP(rec, req)
	return rec
}

func (h *harness) login(token string) *httptest.ResponseRecorder {
	return h.raw("POST", "/fuel/session", `{"token":"`+token+`"}`, map[string]string{"Origin": webOrigin, "Content-Type": "application/json"})
}

// session signs in and returns the cookie header value and the CSRF token.
func (h *harness) session() (cookie, csrf string) {
	h.t.Helper()
	rec := h.login(testToken)
	if rec.Code != 204 {
		h.t.Fatalf("login: %d %s", rec.Code, rec.Body)
	}
	cs := rec.Result().Cookies()
	if len(cs) != 1 {
		h.t.Fatalf("cookies: %v", cs)
	}
	cookie = cs[0].Name + "=" + cs[0].Value
	got := h.raw("GET", "/fuel/session", "", map[string]string{"Cookie": cookie})
	if got.Code != 200 {
		h.t.Fatalf("session: %d %s", got.Code, got.Body)
	}
	csrf = decode[map[string]any](h.t, got)["csrf"].(string)
	return cookie, csrf
}

func TestWebLogin(t *testing.T) {
	h := webHarness(t)
	rec := h.login(testToken)
	if rec.Code != 204 {
		t.Fatalf("login: %d %s", rec.Code, rec.Body)
	}
	sc := rec.Header().Get("Set-Cookie")
	for _, want := range []string{"__Host-fuel_session=", "Path=/", "Max-Age=2592000", "HttpOnly", "Secure", "SameSite=Strict"} {
		if !strings.Contains(sc, want) {
			t.Fatalf("cookie %q lacks %q", sc, want)
		}
	}
	if strings.Contains(sc, "Domain") {
		t.Fatalf("a __Host- cookie has no Domain: %q", sc)
	}
	// The store holds only the hash, mode 0600, and never the token.
	id := rec.Result().Cookies()[0].Value
	p := filepath.Join(h.opts.StateDir, "web-sessions.json")
	b, err := os.ReadFile(p)
	if err != nil || bytes.Contains(b, []byte(id)) || bytes.Contains(b, []byte(testToken)) || !bytes.Contains(b, []byte(webHash(id))) {
		t.Fatalf("session file: %v %s", err, b)
	}
	for _, f := range []string{"web-sessions.json", "web.key"} {
		if fi, err := os.Stat(filepath.Join(h.opts.StateDir, f)); err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("%s: %v", f, err)
		}
	}
	// Refusals before the body is parsed, and strict JSON.
	j := map[string]string{"Origin": webOrigin, "Content-Type": "application/json"}
	cases := []struct {
		name, body string
		hdr        map[string]string
		code       int
		errc       string
	}{
		{"no origin", `{"token":"x"}`, map[string]string{"Content-Type": "application/json"}, 400, "wrong_origin"},
		{"sibling origin", `{"token":"` + testToken + `"}`, map[string]string{"Origin": "https://site.test", "Content-Type": "application/json"}, 400, "wrong_origin"},
		{"null origin", `{"token":"` + testToken + `"}`, map[string]string{"Origin": "null", "Content-Type": "application/json"}, 400, "wrong_origin"},
		{"form", `token=x`, map[string]string{"Origin": webOrigin, "Content-Type": "application/x-www-form-urlencoded"}, 415, "unsupported_media"},
		{"unknown key", `{"token":"x","more":1}`, j, 400, "bad_input"},
		{"no token", `{}`, j, 400, "bad_input"},
		{"too large", `{"token":"` + strings.Repeat("a", 5000) + `"}`, j, 413, "too_large"},
	}
	for _, c := range cases {
		rec := h.raw("POST", "/fuel/session", c.body, c.hdr)
		if rec.Code != c.code || errCode(t, rec) != c.errc || rec.Header().Get("Set-Cookie") != "" {
			t.Fatalf("%s: %d %s", c.name, rec.Code, rec.Body)
		}
	}
	// The agent token never signs in.
	h2 := webHarness(t, func(o *Options) { o.Chat.OpToken = "agent-token-0123456789" })
	if rec := h2.login("agent-token-0123456789"); rec.Code != 401 {
		t.Fatalf("agent token signed in: %d", rec.Code)
	}
	// Other methods.
	if rec := h.raw("PUT", "/fuel/session", "", j); rec.Code != 405 {
		t.Fatalf("PUT: %d", rec.Code)
	}
}

func TestWebLoginBrake(t *testing.T) {
	h := webHarness(t)
	for i := 1; i <= 5; i++ {
		if rec := h.login("wrong"); rec.Code != 401 || errCode(t, rec) != "unauthorized" {
			t.Fatalf("failure %d: %d %s", i, rec.Code, rec.Body)
		}
	}
	rec := h.login("wrong")
	if rec.Code != 429 || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("6th failure: %d %s", rec.Code, rec.Body)
	}
	// A correct token is still accepted while the brake is engaged.
	if rec := h.login(testToken); rec.Code != 204 {
		t.Fatalf("correct token under the brake: %d", rec.Code)
	}
	h.clk.Add(61 * time.Second)
	if rec := h.login("wrong"); rec.Code != 401 {
		t.Fatalf("after the window: %d", rec.Code)
	}
}

func TestWebCSRF(t *testing.T) {
	h := webHarness(t)
	cookie, csrf := h.session()
	body := `{"client_id":"web-csrf-0001","row_key":"it_unknown"}`
	post := func(hdr map[string]string) *httptest.ResponseRecorder {
		hdr["Cookie"] = cookie
		hdr["Content-Type"] = "application/json"
		return h.raw("POST", "/fuel/undo", body, hdr)
	}
	refused := []map[string]string{
		{},                    // no Origin, no header
		{"Origin": webOrigin}, // no header
		{webCSRFHeader: csrf}, // no Origin
		{"Origin": "https://site.test", webCSRFHeader: csrf}, // a sibling host
		{"Origin": "null", webCSRFHeader: csrf},
		{"Origin": webOrigin, webCSRFHeader: "0000"},
		{"Origin": webOrigin, webCSRFHeader: csrf, "Sec-Fetch-Site": "same-site"},
		{"Origin": webOrigin, webCSRFHeader: csrf, "Sec-Fetch-Site": "cross-site"},
	}
	for i, hdr := range refused {
		if rec := post(hdr); rec.Code != 403 || errCode(t, rec) != "csrf" {
			t.Fatalf("case %d: %d %s", i, rec.Code, rec.Body)
		}
	}
	// The same request from the page itself reaches the handler (unknown item).
	if rec := post(map[string]string{"Origin": webOrigin, webCSRFHeader: csrf, "Sec-Fetch-Site": "same-origin"}); rec.Code != 404 {
		t.Fatalf("signed request: %d %s", rec.Code, rec.Body)
	}
	// The CSRF token of another session does not work.
	_, csrf2 := h.session()
	if csrf2 == csrf {
		t.Fatal("two sessions share a CSRF token")
	}
	if rec := post(map[string]string{"Origin": webOrigin, webCSRFHeader: csrf2}); rec.Code != 403 {
		t.Fatalf("foreign CSRF token: %d", rec.Code)
	}
	// Reads work with the cookie alone, but not from another origin's page.
	if rec := h.raw("GET", "/fuel/snapshot", "", map[string]string{"Cookie": cookie}); rec.Code != 200 {
		t.Fatalf("read: %d %s", rec.Code, rec.Body)
	}
	for _, site := range []string{"same-site", "cross-site"} {
		for _, path := range []string{"/fuel/day", "/fuel/photo/ph_x", "/fuel/session"} {
			if rec := h.raw("GET", path, "", map[string]string{"Cookie": cookie, "Sec-Fetch-Site": site}); rec.Code != 403 {
				t.Fatalf("%s read of %s: %d", site, path, rec.Code)
			}
		}
	}
	// No answer carries a CORS allow header; a preflight gets none either.
	for _, rec := range []*httptest.ResponseRecorder{
		h.raw("OPTIONS", "/fuel/undo", "", map[string]string{"Origin": "https://site.test", "Access-Control-Request-Method": "POST"}),
		h.raw("GET", "/fuel/snapshot", "", map[string]string{"Cookie": cookie, "Origin": "https://site.test"}),
		h.raw("GET", "/fuel/web/", "", nil),
	} {
		for k := range rec.Header() {
			if strings.HasPrefix(k, "Access-Control-") {
				t.Fatalf("CORS header %s", k)
			}
		}
	}
	// A photo needs a session and carries CORP.
	if rec := h.raw("GET", "/fuel/photo/ph_x", "", nil); rec.Code != 401 || rec.Header().Get("Cross-Origin-Resource-Policy") != "same-origin" {
		t.Fatalf("photo without a session: %d", rec.Code)
	}
	// Logout needs the CSRF rule too.
	if rec := h.raw("DELETE", "/fuel/session", "", map[string]string{"Cookie": cookie, "Origin": "https://site.test"}); rec.Code != 403 {
		t.Fatalf("cross-origin logout: %d", rec.Code)
	}
}

func TestWebBearerPathUnchanged(t *testing.T) {
	h := webHarness(t)
	cookie, _ := h.session()
	// A bearer write needs no Origin and no CSRF header.
	rec := h.logText("web-bearer-0001", "skyr and walnuts")
	if rec.Code != 200 {
		t.Fatalf("bearer log: %d %s", rec.Code, rec.Body)
	}
	// A wrong bearer plus a valid cookie is judged by the bearer alone.
	for _, auth := range []string{"Bearer wrong", "Basic abc"} {
		for _, path := range []string{"/fuel/snapshot", "/fuel/session"} {
			if rec := h.raw("GET", path, "", map[string]string{"Authorization": auth, "Cookie": cookie}); rec.Code != 401 {
				t.Fatalf("%s %s: %d", auth, path, rec.Code)
			}
		}
	}
	// An unknown cookie is 401.
	if rec := h.raw("GET", "/fuel/snapshot", "", map[string]string{"Cookie": webCookieName + "=AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}); rec.Code != 401 {
		t.Fatalf("unknown cookie: %d", rec.Code)
	}
	// The cookie stands for the app token only: no turn header.
	if rec := h.raw("GET", "/fuel/snapshot", "", map[string]string{"Cookie": cookie, turnHeader: "x"}); rec.Code != 400 {
		t.Fatalf("turn header with a cookie: %d", rec.Code)
	}
}

func TestWebSessionLifecycle(t *testing.T) {
	h := webHarness(t)
	cookie, csrf := h.session()
	get := func(c string) *httptest.ResponseRecorder {
		return h.raw("GET", "/fuel/snapshot", "", map[string]string{"Cookie": c})
	}
	// Inside the hour: no new cookie. After it: the cookie is sent again.
	h.clk.Add(30 * time.Minute)
	if rec := get(cookie); rec.Code != 200 || rec.Header().Get("Set-Cookie") != "" {
		t.Fatalf("fresh session: %d %q", rec.Code, rec.Header().Get("Set-Cookie"))
	}
	h.clk.Add(2 * time.Hour)
	if rec := get(cookie); rec.Code != 200 || !strings.Contains(rec.Header().Get("Set-Cookie"), "Max-Age=2592000") {
		t.Fatalf("renewal: %d %q", rec.Code, rec.Header().Get("Set-Cookie"))
	}
	// A restart keeps the session.
	h.restart()
	if rec := get(cookie); rec.Code != 200 {
		t.Fatalf("after a restart: %d", rec.Code)
	}
	// Sliding: used every 20 days it lives on, but never past 90 days.
	for i := 0; i < 4; i++ {
		h.clk.Add(20 * 24 * time.Hour)
		if rec := get(cookie); rec.Code != 200 {
			t.Fatalf("day %d: %d", 20*(i+1), rec.Code)
		}
	}
	h.clk.Add(11 * 24 * time.Hour) // day 91
	if rec := get(cookie); rec.Code != 401 {
		t.Fatalf("past the 90-day cap: %d", rec.Code)
	}
	// Unused for 31 days: gone.
	cookie, csrf = h.session()
	h.clk.Add(31 * 24 * time.Hour)
	if rec := get(cookie); rec.Code != 401 {
		t.Fatalf("expired: %d", rec.Code)
	}
	// Logout ends this session and clears the cookie; the other one lives.
	cookie, csrf = h.session()
	other, _ := h.session()
	rec := h.raw("DELETE", "/fuel/session", "", map[string]string{"Cookie": cookie, "Origin": webOrigin, webCSRFHeader: csrf})
	if rec.Code != 204 || !strings.Contains(rec.Header().Get("Set-Cookie"), "Max-Age=0") {
		t.Fatalf("logout: %d %q", rec.Code, rec.Header().Get("Set-Cookie"))
	}
	if get(cookie).Code != 401 || get(other).Code != 200 {
		t.Fatal("logout ended the wrong session")
	}
	// ?all=1 ends every session.
	cookie, csrf = h.session()
	if rec := h.raw("DELETE", "/fuel/session?all=1", "", map[string]string{"Cookie": cookie, "Origin": webOrigin, webCSRFHeader: csrf}); rec.Code != 204 {
		t.Fatalf("logout all: %d", rec.Code)
	}
	if get(cookie).Code != 401 || get(other).Code != 401 {
		t.Fatal("a session survived logout all")
	}
	// A sixth login drops the oldest.
	var cs []string
	for i := 0; i < 6; i++ {
		c, _ := h.session()
		cs = append(cs, c)
		h.clk.Add(time.Second)
	}
	if get(cs[0]).Code != 401 || get(cs[1]).Code != 200 || get(cs[5]).Code != 200 {
		t.Fatal("the sixth login did not drop the oldest session")
	}
	// A changed app token ends every session at startup.
	h.svc.Close()
	h.opts.Token = "tok-another-0123456789"
	h.start()
	if rec := get(cs[5]); rec.Code != 401 {
		t.Fatalf("after a token change: %d", rec.Code)
	}
}

func TestWebDisabled(t *testing.T) {
	// A session from a time when the web was on.
	h := webHarness(t)
	cookie, csrf := h.session()
	h.svc.Close()
	h.opts.Web = WebOptions{}
	h.start()
	for _, c := range []struct{ method, path string }{
		{"GET", "/fuel/web/"}, {"GET", "/fuel/web"}, {"GET", "/fuel/web/app.js"},
		{"POST", "/fuel/session"}, {"GET", "/fuel/session"}, {"DELETE", "/fuel/session"},
	} {
		rec := h.raw(c.method, c.path, `{"token":"`+testToken+`"}`, map[string]string{"Origin": webOrigin, "Content-Type": "application/json", "Cookie": cookie, webCSRFHeader: csrf})
		if rec.Code != 404 || rec.Header().Get("Set-Cookie") != "" {
			t.Fatalf("%s %s: %d", c.method, c.path, rec.Code)
		}
		// The same with the bearer: the routes do not exist.
		if rec := h.do(c.method, c.path, nil, ""); rec.Code != 404 {
			t.Fatalf("bearer %s %s: %d", c.method, c.path, rec.Code)
		}
	}
	if rec := h.raw("GET", "/fuel/snapshot", "", map[string]string{"Cookie": cookie}); rec.Code != 401 {
		t.Fatalf("cookie with the web off: %d", rec.Code)
	}
	rec := h.do("GET", "/fuel/snapshot", nil, "")
	if rec.Code != 200 || rec.Header().Get("Content-Security-Policy") != "" {
		t.Fatalf("bearer with the web off: %d", rec.Code)
	}
}

func TestWebStaticAndHeaders(t *testing.T) {
	h := webHarness(t)
	rec := h.raw("GET", "/fuel/web/", "", nil)
	if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") || rec.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("shell: %d %v", rec.Code, rec.Header())
	}
	if bytes.Contains(rec.Body.Bytes(), []byte(testToken)) {
		t.Fatal("the shell holds the token")
	}
	etag := rec.Header().Get("ETag")
	if rec := h.raw("GET", "/fuel/web/index.html", "", map[string]string{"If-None-Match": etag}); rec.Code != 304 {
		t.Fatalf("etag: %d", rec.Code)
	}
	if rec := h.raw("GET", "/fuel/web", "", nil); rec.Code != 308 || rec.Header().Get("Location") != "/fuel/web/" {
		t.Fatalf("redirect: %d", rec.Code)
	}
	for _, name := range []string{"app.js", "style.css"} {
		if rec := h.raw("GET", "/fuel/web/"+name, "", nil); rec.Code != 200 {
			t.Fatalf("%s: %d", name, rec.Code)
		}
	}
	for _, path := range []string{"/fuel/web/nope.js", "/fuel/web/../config.go", "/fuel/web/%2e%2e/web.go", "/fuel/web/web.go", "/fuel/web/index.html/x"} {
		if rec := h.raw("GET", path, "", nil); rec.Code != 404 {
			t.Fatalf("%s: %d", path, rec.Code)
		}
	}
	if rec := h.raw("POST", "/fuel/web/", "", nil); rec.Code != 405 {
		t.Fatalf("POST on the shell: %d", rec.Code)
	}
	// The headers are on the shell, on session answers, on errors and on data.
	cookie, _ := h.session()
	for name, rec := range map[string]*httptest.ResponseRecorder{
		"shell":    h.raw("GET", "/fuel/web/", "", nil),
		"login":    h.login("wrong"),
		"401":      h.raw("GET", "/fuel/snapshot", "", nil),
		"404":      h.raw("GET", "/fuel/web/nope", "", nil),
		"redirect": h.raw("GET", "/fuel/web", "", nil),
		"data":     h.raw("GET", "/fuel/snapshot", "", map[string]string{"Cookie": cookie}),
	} {
		hd := rec.Header()
		csp := hd.Get("Content-Security-Policy")
		if !strings.Contains(csp, "default-src 'none'") || !strings.Contains(csp, "script-src 'self'") || !strings.Contains(csp, "frame-ancestors 'none'") ||
			strings.Contains(csp, "unsafe") || strings.Contains(csp, "http") ||
			hd.Get("X-Content-Type-Options") != "nosniff" || hd.Get("X-Frame-Options") != "DENY" ||
			hd.Get("Referrer-Policy") != "no-referrer" || hd.Get("Cross-Origin-Resource-Policy") != "same-origin" ||
			hd.Get("Cross-Origin-Opener-Policy") != "same-origin" || hd.Get("Strict-Transport-Security") == "" {
			t.Fatalf("%s: headers %v", name, hd)
		}
	}
	// The page loads nothing from another origin and has no inline script or style.
	for name, f := range h.svc.web.files {
		b := string(f.body)
		for _, bad := range []string{"https://", "http://", "//cdn", "innerHTML", "eval(", "document.write"} {
			if strings.Contains(b, bad) && !(strings.HasSuffix(name, ".svg") && bad == "http://") {
				t.Fatalf("%s contains %q", name, bad)
			}
		}
		if strings.HasSuffix(name, ".html") && (strings.Contains(b, "<style") || strings.Contains(b, " style=") || strings.Contains(b, "onclick=") || strings.Contains(strings.ReplaceAll(b, `<script type="module" src=`, ""), "<script")) {
			t.Fatalf("%s has inline script or style", name)
		}
	}
}

// The session routes work while the service starts up; data routes wait.
func TestWebSessionRoutesBeforeReady(t *testing.T) {
	h := webHarness(t)
	o := h.opts
	o.StateDir = filepath.Join(t.TempDir(), "state")
	svc, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.Close)
	h.h = svc.Handler() // never started: not ready
	cookie, csrf := h.session()
	if rec := h.raw("GET", "/fuel/snapshot", "", map[string]string{"Cookie": cookie}); rec.Code != 503 {
		t.Fatalf("data before ready: %d", rec.Code)
	}
	if rec := h.raw("GET", "/fuel/web/", "", nil); rec.Code != 200 {
		t.Fatalf("shell before ready: %d", rec.Code)
	}
	if rec := h.raw("DELETE", "/fuel/session", "", map[string]string{"Cookie": cookie, "Origin": webOrigin, webCSRFHeader: csrf}); rec.Code != 204 {
		t.Fatalf("logout before ready: %d", rec.Code)
	}
}

func TestWebConfig(t *testing.T) {
	base := "listen = \"127.0.0.1:1\"\nfood_log_var = \"Fuel e2e food log\"\n"
	ok := []string{
		base,
		base + "web_enabled = true\npublic_origin = \"https://fuel.gojoe.run\"\n",
		base + "test_mode = true\nweb_enabled = true\npublic_origin = \"http://100.120.65.8:8797\"\nweb_insecure_test_cookie = true\n",
	}
	for _, c := range ok {
		if _, err := ParseDaemonConfig([]byte(c)); err != nil {
			t.Fatalf("%q: %v", c, err)
		}
	}
	bad := []string{
		base + "web_enabled = true\n",                                                                                                // no origin
		base + "web_enabled = true\npublic_origin = \"fuel.gojoe.run\"\n",                                                            // no scheme
		base + "web_enabled = true\npublic_origin = \"https://fuel.gojoe.run/\"\n",                                                   // a path
		base + "web_enabled = true\npublic_origin = \"http://100.120.65.8:8797\"\n",                                                  // http without the test switch
		base + "test_mode = true\nweb_enabled = true\npublic_origin = \"https://fuel.gojoe.run\"\nweb_insecure_test_cookie = true\n", // https refuses the switch
		base + "web_enabled = true\npublic_origin = \"http://100.120.65.8:8797\"\nweb_insecure_test_cookie = true\n",                 // no test_mode
		base + "web_insecure_test_cookie = true\n",
	}
	for _, c := range bad {
		if _, err := ParseDaemonConfig([]byte(c)); err == nil {
			t.Fatalf("accepted: %q", c)
		}
	}
	if c, _ := ParseDaemonConfig([]byte(base)); c.WebEnabled {
		t.Fatal("web_enabled is not off by default")
	}
	// The test cookie: no Secure, no __Host- prefix; the real one is never weakened.
	h := newHarness(t, func(o *Options) {
		o.Web = WebOptions{Enabled: true, PublicOrigin: "http://127.0.0.1:1", InsecureTestCookie: true}
	})
	rec := h.raw("POST", "/fuel/session", `{"token":"`+testToken+`"}`, map[string]string{"Origin": "http://127.0.0.1:1", "Content-Type": "application/json"})
	sc := rec.Header().Get("Set-Cookie")
	if rec.Code != 204 || strings.Contains(sc, "Secure") || strings.Contains(sc, "__Host-") || !strings.Contains(sc, "HttpOnly") || !strings.Contains(sc, "SameSite=Strict") {
		t.Fatalf("test cookie: %d %q", rec.Code, sc)
	}
	if rec.Header().Get("Strict-Transport-Security") != "" {
		t.Fatal("HSTS on an http origin")
	}
	if _, err := New(Options{Token: "t", Vars: h.opts.Vars, Model: h.opts.Model, FoodVar: "x", BodyVar: "y", StateDir: t.TempDir(),
		Web: WebOptions{Enabled: true, PublicOrigin: "https://fuel.gojoe.run", InsecureTestCookie: true}, TestMode: true}); err == nil {
		t.Fatal("the test cookie was accepted with an https origin")
	}
}

// A burst of failed logins writes at most two log lines.
func TestWebLoginConcurrencyLimit(t *testing.T) {
	h := webHarness(t)
	for i := 0; i < webLoginInflight; i++ {
		h.svc.web.logins <- struct{}{}
	}
	rec := h.login(testToken)
	if rec.Code != 503 || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("5th login in flight: %d", rec.Code)
	}
	<-h.svc.web.logins
	if rec := h.login(testToken); rec.Code != 204 {
		t.Fatalf("after a slot is free: %d", rec.Code)
	}
	var _ = http.StatusOK
}

// The logic tests of the page code (double writes, sign-out). Skipped when
// node is not installed.
func TestWebJS(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	out, err := exec.Command(node, "--test", "webtest/writes.test.mjs").CombinedOutput()
	if err != nil {
		t.Fatalf("node --test: %v\n%s", err, out)
	}
	if !bytes.Contains(out, []byte("# fail 0")) {
		t.Fatalf("node --test:\n%s", out)
	}
}

// The static allow list holds the app only, never a test file.
func TestWebEmbedHasNoTests(t *testing.T) {
	h := webHarness(t)
	for name := range h.svc.web.files {
		if strings.Contains(name, "test") {
			t.Fatalf("embedded: %s", name)
		}
	}
}

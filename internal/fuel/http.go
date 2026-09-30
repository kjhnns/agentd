package fuel

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"mime"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	maxBody      = 24 << 20
	maxTextChars = 2000
	maxAudio     = 10 << 20
	maxImage     = 8 << 20
	maxImages    = 4
	hourCap      = 30
	dayCap       = 200
)

// apiError is the one error shape (spec 10).
type apiError struct {
	status     int
	code       string
	msg        string
	retryable  bool
	retryAfter int
}

func (e *apiError) Error() string { return e.code + ": " + e.msg }

func errf(status int, code string, retry bool, format string, a ...any) *apiError {
	return &apiError{status: status, code: code, msg: fmt.Sprintf(format, a...), retryable: retry}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, e *apiError) {
	if e.retryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(e.retryAfter))
	}
	writeJSON(w, e.status, map[string]any{"error": map[string]any{"code": e.code, "message": e.msg, "retryable": e.retryable}})
}

// ---- auth (checked BEFORE any body read) ----

type authFailures struct {
	mu     sync.Mutex
	window time.Time
	count  int
}

const (
	authFailLimit  = 30
	authFailWindow = time.Minute
)

func (f *authFailures) note(now time.Time) (engaged bool, retryAfter int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if now.Sub(f.window) > authFailWindow {
		f.window, f.count = now, 0
	}
	f.count++
	ra := int(math.Ceil((authFailWindow - now.Sub(f.window)).Seconds()))
	if ra < 1 {
		ra = 1
	}
	return f.count > authFailLimit, ra
}

func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		return strings.TrimSpace(strings.Split(xff, ",")[0])
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (s *Service) authed(r *http.Request) bool {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		return false
	}
	got := strings.TrimPrefix(h, "Bearer ")
	return got != "" && s.o.Token != "" && subtle.ConstantTimeCompare([]byte(got), []byte(s.o.Token)) == 1
}

func (s *Service) gate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if !s.authed(r) {
			if engaged, ra := s.fails.note(s.o.Now()); engaged {
				log.Printf("fuel: auth brake engaged, last from %s", clientIP(r))
				e := errf(http.StatusTooManyRequests, "rate_limited", true, "too many failed attempts")
				e.retryAfter = ra
				writeErr(w, e)
				return
			}
			log.Printf("fuel: rejected %s %s from %s", r.Method, r.URL.Path, clientIP(r))
			writeErr(w, errf(http.StatusUnauthorized, "unauthorized", false, "missing or wrong token"))
			return
		}
		s.mu.Lock()
		ready := s.ready
		s.mu.Unlock()
		if !ready {
			e := errf(http.StatusServiceUnavailable, "busy", true, "starting up")
			e.retryAfter = 5
			writeErr(w, e)
			return
		}
		if _, err := s.loadTargets(); err != nil {
			writeErr(w, errf(http.StatusServiceUnavailable, "targets_invalid", false, "the targets file is invalid; fix it on the server"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Handler serves /fuel/*. Mount it with api.MountPublic: it carries its own
// token gate, and the API bearer is neither required nor accepted.
func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /fuel/log", s.handleLog)
	mux.HandleFunc("POST /fuel/undo", func(w http.ResponseWriter, r *http.Request) { s.handleMutation(w, r, "undo") })
	mux.HandleFunc("POST /fuel/fraction", func(w http.ResponseWriter, r *http.Request) { s.handleMutation(w, r, "fraction") })
	mux.HandleFunc("GET /fuel/entry/{id}", s.handleEntry)
	mux.HandleFunc("GET /fuel/feed", s.handleFeed)
	mux.HandleFunc("GET /fuel/photo/{id}", s.handlePhoto)
	mux.HandleFunc("GET /fuel/snapshot", s.handleSnapshot)
	mux.HandleFunc("/fuel/", func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, errf(http.StatusNotFound, "not_found", false, "no such route"))
	})
	return s.gate(mux)
}

// ---- idempotency + concurrency ----

var clientIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{7,63}$`)

// idemResult is what an idempotency check decided.
type idemResult struct {
	replay  *idemRec // a stored record to answer from
	release func()   // call when this request is finished (wakes waiters)
}

// lookupIdem finds a request identity younger than 7 days: the idem store
// (which may hold the final response) first, else the journal's own record
// of the identity (durable even when idem.jsonl could not be written).
func (s *Service) lookupIdem(clientID string) (idemRec, bool) {
	now := s.o.Now()
	if rec, ok := s.idem.Get(clientID); ok && now.Sub(rec.At) <= idemRetention {
		return rec, true
	}
	if id, ok := s.journal.Ident(clientID); ok && now.Sub(id.At) <= idemRetention {
		return idemRec{ClientID: clientID, Hash: id.Hash, Kind: id.Kind, At: id.At, EntryID: id.EntryID, ItemID: id.ItemID}, true
	}
	return idemRec{}, false
}

// claim implements spec 6: same client_id + same hash returns the stored
// response (or the in-progress state); a different hash is 409; a concurrent
// duplicate waits for the first.
func (s *Service) claim(ctx context.Context, clientID, kind, hash string) (idemResult, *apiError) {
	for {
		s.reqMu.Lock()
		// An ACTIVE request with this client_id wins over any durable state:
		// a duplicate waits for it to finish (spec 6), even when its journal
		// identity or reservation is already visible.
		if cur, ok := s.inflReq[clientID]; ok {
			s.reqMu.Unlock()
			if cur.kind != kind || cur.hash != hash {
				return idemResult{}, errf(http.StatusConflict, "idempotency_conflict", false, "client_id is in use by a different request")
			}
			select {
			case <-cur.done:
				continue // the first finished: re-check
			case <-ctx.Done():
				e := errf(http.StatusServiceUnavailable, "busy", true, "a request with this client_id is still running")
				e.retryAfter = 2
				return idemResult{}, e
			}
		}
		// Durable state, checked UNDER the claim lock: a first request stores
		// its record before it releases its claim.
		if rec, ok := s.lookupIdem(clientID); ok {
			s.reqMu.Unlock()
			if rec.Kind != kind || rec.Hash != hash {
				return idemResult{}, errf(http.StatusConflict, "idempotency_conflict", false, "client_id was used for a different request")
			}
			return idemResult{replay: &rec}, nil
		}
		ir := &inflightReq{hash: hash, kind: kind, done: make(chan struct{})}
		s.inflReq[clientID] = ir
		s.reqMu.Unlock()
		var once sync.Once
		return idemResult{release: func() {
			once.Do(func() {
				s.reqMu.Lock()
				delete(s.inflReq, clientID)
				s.reqMu.Unlock()
				close(ir.done)
			})
		}}, nil
	}
}

// rateCheck enforces 30 logs an hour and 200 a day; ok logs count.
func (s *Service) rateCheck(now time.Time) *apiError {
	s.reqMu.Lock()
	defer s.reqMu.Unlock()
	keep := s.logTimes[:0]
	hour := 0
	var oldestHour time.Time
	for _, t := range s.logTimes {
		if now.Sub(t) < 24*time.Hour {
			keep = append(keep, t)
			if now.Sub(t) < time.Hour {
				if hour == 0 {
					oldestHour = t
				}
				hour++
			}
		}
	}
	s.logTimes = keep
	if hour >= hourCap || len(keep) >= dayCap {
		ra := 60
		if hour >= hourCap {
			ra = int(time.Hour.Seconds() - now.Sub(oldestHour).Seconds() + 1)
		} else if len(keep) > 0 {
			ra = int(24*time.Hour.Seconds() - now.Sub(keep[0]).Seconds() + 1)
		}
		e := errf(http.StatusTooManyRequests, "rate_limited", true, "log limit reached (30 an hour, 200 a day)")
		e.retryAfter = ra
		return e
	}
	s.logTimes = append(s.logTimes, now)
	return nil
}

func (s *Service) acquireSlot(ctx context.Context) *apiError {
	t := time.NewTimer(s.o.BusyWait)
	defer t.Stop()
	select {
	case s.slots <- struct{}{}:
		return nil
	case <-t.C:
	case <-ctx.Done():
	}
	e := errf(http.StatusServiceUnavailable, "busy", true, "two logs are already being processed")
	e.retryAfter = 5
	return e
}

// ---- POST /fuel/log ----

type logInput struct {
	ClientID  string
	Text      string
	LocalTime string
	Audio     []byte
	Images    [][]byte
}

func (in logInput) hash() string {
	h := sha256.New()
	fmt.Fprintf(h, "log\x00%s\x00%s\x00", strings.TrimSpace(in.Text), strings.TrimSpace(in.LocalTime))
	a := sha256.Sum256(in.Audio)
	if len(in.Audio) == 0 {
		a = [32]byte{}
	}
	h.Write(a[:])
	for _, im := range in.Images {
		x := sha256.Sum256(im)
		h.Write(x[:])
	}
	return hex.EncodeToString(h.Sum(nil))
}

func maxBytesErr(err error) bool {
	var mb *http.MaxBytesError
	return errors.As(err, &mb)
}

// readLogInput reads the multipart (or text-only JSON) body within the caps.
func (s *Service) readLogInput(w http.ResponseWriter, r *http.Request) (logInput, *apiError) {
	var in logInput
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(s.o.ReadDeadline))
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	tooBig := errf(http.StatusRequestEntityTooLarge, "too_large", false, "request over the size limits")
	switch {
	case ct == "multipart/form-data":
		mr, err := r.MultipartReader()
		if err != nil {
			return in, errf(http.StatusBadRequest, "bad_input", false, "bad multipart body")
		}
		for {
			p, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				if maxBytesErr(err) {
					return in, tooBig
				}
				return in, errf(http.StatusBadRequest, "bad_input", false, "bad multipart body")
			}
			name := p.FormName()
			readCap := func(limit int64) ([]byte, *apiError) {
				b, err := io.ReadAll(io.LimitReader(p, limit+1))
				if err != nil {
					if maxBytesErr(err) {
						return nil, tooBig
					}
					return nil, errf(http.StatusBadRequest, "bad_input", false, "upload interrupted")
				}
				if int64(len(b)) > limit {
					return nil, tooBig
				}
				return b, nil
			}
			switch name {
			case "client_id", "text", "local_time":
				b, e := readCap(16 << 10)
				if e != nil {
					if name == "text" {
						return in, tooBig
					}
					return in, errf(http.StatusBadRequest, "bad_input", false, "%s too long", name)
				}
				switch name {
				case "client_id":
					in.ClientID = strings.TrimSpace(string(b))
				case "text":
					in.Text = string(b)
				case "local_time":
					in.LocalTime = strings.TrimSpace(string(b))
				}
			case "audio":
				if in.Audio != nil {
					return in, errf(http.StatusBadRequest, "bad_input", false, "at most one audio part")
				}
				b, e := readCap(maxAudio)
				if e != nil {
					return in, e
				}
				in.Audio = b
			case "image":
				if len(in.Images) >= maxImages {
					return in, errf(http.StatusBadRequest, "bad_input", false, "at most %d images", maxImages)
				}
				b, e := readCap(maxImage)
				if e != nil {
					return in, e
				}
				in.Images = append(in.Images, b)
			default:
				return in, errf(http.StatusBadRequest, "bad_input", false, "unknown part %q", name)
			}
			p.Close()
		}
	case ct == "application/json":
		var body struct {
			ClientID  string `json:"client_id"`
			Text      string `json:"text"`
			LocalTime string `json:"local_time"`
		}
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		if err := decodeStrict(r.Body, &body); err != nil {
			if maxBytesErr(err) {
				return in, tooBig
			}
			return in, errf(http.StatusBadRequest, "bad_input", false, "bad JSON body (one object; text-only logs; use multipart for audio or images)")
		}
		in.ClientID, in.Text, in.LocalTime = strings.TrimSpace(body.ClientID), body.Text, strings.TrimSpace(body.LocalTime)
	default:
		return in, errf(http.StatusUnsupportedMediaType, "unsupported_media", false, "use multipart/form-data or application/json")
	}
	_ = rc.SetReadDeadline(time.Time{})
	in.Text = strings.TrimSpace(in.Text)
	if !clientIDRe.MatchString(in.ClientID) {
		return in, errf(http.StatusBadRequest, "bad_input", false, "client_id (a uuid) is required")
	}
	if utf8.RuneCountInString(in.Text) > maxTextChars {
		return in, errf(http.StatusRequestEntityTooLarge, "too_large", false, "text over %d characters", maxTextChars)
	}
	if in.Text == "" && in.Audio == nil && len(in.Images) == 0 {
		return in, errf(http.StatusBadRequest, "empty_input", false, "give text, audio or a photo")
	}
	return in, nil
}

// decodeStrict reads the WHOLE (capped) body and requires exactly one JSON
// object with known fields and nothing after it.
func decodeStrict(r io.Reader, v any) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		return errors.New("trailing data after the JSON object")
	}
	return nil
}

// LogResponse is the 200/202 body of POST /fuel/log (spec 10).
type LogResponse struct {
	Status     string         `json:"status"`
	EntryID    string         `json:"entry_id"`
	Intent     string         `json:"intent"`
	Transcript *string        `json:"transcript"`
	PhotoIDs   []string       `json:"photo_ids"`
	Items      []ItemState    `json:"items"`
	Blocks     []Block        `json:"blocks"`
	Snapshot   Snapshot       `json:"snapshot"`
	LatencyMs  map[string]int `json:"latency_ms"`
	renderErr  error          // the render failed (targets invalid): never sent or stored
}

func ms(d time.Duration) int { return int(d / time.Millisecond) }

func (s *Service) handleLog(w http.ResponseWriter, r *http.Request) {
	t0 := time.Now()
	if e := s.acquireSlot(r.Context()); e != nil {
		writeErr(w, e)
		return
	}
	released := false
	release := func() {
		if !released {
			released = true
			<-s.slots
		}
	}
	defer release()

	in, e := s.readLogInput(w, r)
	if e != nil {
		writeErr(w, e)
		return
	}
	upload := time.Since(t0)
	// The 30 s clock starts after the body is read; the work is detached from
	// the client connection so a disconnect never strands a started write.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), s.o.Budget)
	defer cancel()
	now := s.o.Now()

	hash := in.hash()
	claim, ce := s.claim(ctx, in.ClientID, "log", hash)
	if ce != nil {
		writeErr(w, ce)
		return
	}
	if claim.replay != nil {
		s.replayLog(ctx, w, *claim.replay)
		return
	}
	defer claim.release()

	targets, terr := s.loadTargets()
	if terr != nil {
		writeErr(w, errf(http.StatusServiceUnavailable, "targets_invalid", false, "targets invalid"))
		return
	}
	eatenAt := now
	if in.LocalTime != "" {
		t, err := time.Parse(time.RFC3339, in.LocalTime)
		if err != nil {
			writeErr(w, errf(http.StatusBadRequest, "bad_input", false, "local_time must be RFC3339 with an offset"))
			return
		}
		if now.Sub(t) > 48*time.Hour || t.Sub(now) > 5*time.Minute {
			writeErr(w, errf(http.StatusBadRequest, "bad_input", false, "local_time more than 48 h in the past or 5 min in the future"))
			return
		}
		eatenAt = t
	}
	date := eatenAt.In(targets.loc).Format("2006-01-02")

	// Validate media before anything costs money.
	var imgs []*processedImage
	for _, raw := range in.Images {
		p, err := processImage(raw)
		switch {
		case errors.Is(err, errTooBig):
			writeErr(w, errf(http.StatusRequestEntityTooLarge, "too_large", false, "image over 30 megapixels"))
			return
		case err != nil:
			writeErr(w, errf(http.StatusUnsupportedMediaType, "unsupported_media", false, "images must be JPEG or PNG (convert HEIC on the client)"))
			return
		}
		imgs = append(imgs, p)
	}
	audioExt := ""
	if in.Audio != nil {
		ext, secs, err := sniffAudio(in.Audio)
		if err != nil {
			writeErr(w, errf(http.StatusUnsupportedMediaType, "unsupported_media", false, "audio must be AAC in MP4 (m4a) or WAV"))
			return
		}
		if secs < 0.5 || secs > 120 {
			writeErr(w, errf(http.StatusBadRequest, "bad_input", false, "audio must be 0.5 to 120 s"))
			return
		}
		audioExt = ext
	}
	if e := s.rateCheck(now); e != nil {
		writeErr(w, e)
		return
	}

	// Freshen the entry's day in parallel with ASR and the model call.
	var fresh sync.WaitGroup
	fresh.Add(1)
	go func() { defer fresh.Done(); s.freshen(ctx, date) }()

	// ASR.
	var transcript *string
	tASR := time.Now()
	if in.Audio != nil {
		txt, e := s.transcribe(ctx, in.Audio, audioExt)
		if e != nil {
			writeErr(w, e)
			return
		}
		transcript = &txt
	}
	asrD := time.Since(tASR)
	if in.Audio != nil && strings.TrimSpace(*transcript) == "" && in.Text == "" && len(imgs) == 0 {
		writeErr(w, errf(http.StatusBadRequest, "empty_input", false, "nothing was heard and no text or photo came"))
		return
	}
	foodText := in.Text
	if transcript != nil && strings.TrimSpace(*transcript) != "" {
		foodText = strings.TrimSpace(*transcript)
		if in.Text != "" {
			foodText += "\n" + in.Text
		}
	}

	// The model call (one; one retry on invalid output).
	tModel := time.Now()
	fresh.Wait() // the model sees the refreshed day (questions answer from it)
	preSnap, _ := s.snapshotFor(date)
	mi := ModelInput{Text: foodText, Staples: s.staples, Snapshot: &preSnap}
	for _, p := range imgs {
		mi.Images = append(mi.Images, p.Model)
	}
	out, e := s.callModel(ctx, mi)
	modelD := time.Since(tModel)
	if e != nil {
		writeErr(w, e)
		return
	}
	if ctx.Err() != nil {
		// Before the first Variables write: nothing is journaled, a retry with
		// the same client_id runs again.
		writeErr(w, errf(http.StatusGatewayTimeout, "timeout", true, "took too long; nothing was written, retry"))
		return
	}

	entry := Entry{ID: newID("en_"), ClientID: in.ClientID, Date: date, EatenAt: eatenAt, CreatedAt: now,
		Intent: out.Intent, Transcript: transcript, PhotoIDs: []string{}, ReqHash: hash}
	items := s.buildItems(out, entry)
	for _, it := range items {
		entry.ItemIDs = append(entry.ItemIDs, it.ID)
	}
	modelText, hadDigits := stripDigits(out.Text)
	if hadDigits {
		log.Printf("fuel: entry %s: digits stripped from the model text", entry.ID)
	}

	timeout := errf(http.StatusGatewayTimeout, "timeout", true, "took too long; nothing was written, retry")
	if ctx.Err() != nil {
		writeErr(w, timeout)
		return
	}
	// Photos are stored only for an accepted request.
	for _, p := range imgs {
		id, err := s.photos.save(p.Stored)
		if err != nil {
			log.Printf("fuel: photo save failed")
			continue
		}
		entry.PhotoIDs = append(entry.PhotoIDs, id)
	}
	photoIDs := entry.PhotoIDs

	// The transition into durable writing: ONE journal txn (entry, items and
	// ops together; the journal is the source of truth), then the idempotency
	// reservation, both BEFORE any external write (spec 6). The deadline is
	// checked right at this boundary: expired = 504, nothing journaled.
	tWrite := time.Now()
	var ops []Op
	if out.Intent == "log" {
		photoRef := strings.Join(photoIDs, ",")
		for _, it := range items {
			opID := newID("op_")
			ops = append(ops, Op{ID: opID, Kind: "original", EntryID: entry.ID, ItemID: it.ID, RowItemID: it.ID, Date: date,
				Data: originalRowData(it, opID, photoRef), Macros: it.Orig, CreatedAt: now, State: OpPending,
				Attempts: 1, LastTry: s.o.Now()})
		}
	}
	entry.UserText, entry.ModelText, entry.Widgets = foodText, modelText, out.Widgets
	if err := s.journal.AppendCtx(ctx, journalRec{T: "txn", Entry: &entry, Items: items, Ops: ops}); err != nil {
		switch {
		case errors.Is(err, ErrDeadline):
			s.photos.remove(photoIDs)
			writeErr(w, timeout)
		case errors.Is(err, ErrJournalBroken) && s.journalHas(entry.ID):
			// Possibly durable: keep the identity, write nothing now; a
			// restart replays and reconciles it. The retry sees the state.
			log.Printf("fuel: journal durability uncertain after entry %s; writes stopped", entry.ID)
			e := errf(http.StatusServiceUnavailable, "busy", true, "storage problem on the server; retry later")
			e.retryAfter = 60
			writeErr(w, e)
		default:
			s.photos.remove(photoIDs)
			log.Printf("fuel: journal txn for %s failed; nothing written", entry.ID)
			writeErr(w, errf(http.StatusInternalServerError, "internal", true, "could not persist the request; nothing was written"))
		}
		return
	}
	if err := s.idem.Put(idemRec{ClientID: in.ClientID, Hash: hash, Kind: "log", At: now, EntryID: entry.ID, ItemIDs: entry.ItemIDs}); err != nil {
		log.Printf("fuel: idem reservation for %s failed (the journal identity still holds)", entry.ID)
	}
	if len(ops) > 0 {
		s.runOps(ctx, ops)
	}
	writeD := time.Since(tWrite)
	fresh.Wait()

	// Status, items and widgets are captured together in ONE render; a
	// render from incomplete history is answered but never persisted (feed,
	// coach event, final response): recovery renders those later.
	ready := s.renderReady(ctx, date)
	resp, status := s.buildLogResponse(entry)
	if resp.renderErr != nil {
		writeErr(w, errf(http.StatusServiceUnavailable, "targets_invalid", false, "targets invalid; the request itself was saved, retry to see it"))
		return
	}
	resp.LatencyMs = map[string]int{"upload": ms(upload), "asr": ms(asrD), "model": ms(modelD), "write": ms(writeD), "total": ms(time.Since(t0))}
	states, snap := resp.Items, resp.Snapshot
	if ready {
		s.appendEntryFeed(entry, resp.Blocks)
	}

	code := http.StatusOK
	switch status {
	case StatusPending:
		code = http.StatusAccepted
	case StatusFailed:
		s.logLine("log", entry.ID, len(items), resp.LatencyMs, "failed")
		writeErr(w, errf(http.StatusBadGateway, "upstream_failed", false, "Variables rejected part of the entry; nothing more is written and the saved part is being cancelled"))
		return
	}
	body, _ := json.Marshal(resp)
	if out.Intent == "log" && status == StatusDone && ready {
		// Before the final response is stored; a pending entry gets its
		// event from the reconciler once it is done, with its real macros.
		s.coachEvent(entry, states, snap)
	}
	if status == StatusDone && ready {
		_ = s.idem.Put(idemRec{ClientID: in.ClientID, Hash: hash, Kind: "log", At: now, EntryID: entry.ID, ItemIDs: entry.ItemIDs, Status: code, Response: body})
	}
	if out.Intent != "log" && ready {
		// Questions write nothing; the idempotency record is the answer.
		_ = s.idem.Put(idemRec{ClientID: in.ClientID, Hash: hash, Kind: "log", At: now, EntryID: entry.ID, Status: code, Response: body})
	}
	s.logLine("log", entry.ID, len(items), resp.LatencyMs, status)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(append(body, '\n'))
}

func (s *Service) logLine(kind, id string, n int, lat map[string]int, status string) {
	log.Printf("fuel: %s %s status=%s items=%d latency_ms upload=%d asr=%d model=%d write=%d total=%d",
		kind, id, status, n, lat["upload"], lat["asr"], lat["model"], lat["write"], lat["total"])
}

// coachEvent appends one line per log to coach-events.jsonl (spec 11),
// once per entry: startup re-adds a line a crash lost (materializeFeed).
func (s *Service) coachEvent(e Entry, states []ItemState, snap Snapshot) {
	s.coachMu.Lock()
	defer s.coachMu.Unlock()
	if s.coachSeen[e.ID] {
		return
	}
	type it struct {
		ItemID string `json:"item_id"`
		Item   string `json:"item"`
		Macros Macros `json:"macros"`
	}
	its := []it{}
	for _, st := range states {
		its = append(its, it{st.ItemID, st.Item, st.Effective})
	}
	totals := map[string]float64{}
	for _, m := range snap.Macros {
		totals[m.Key] = m.Consumed
	}
	b, err := json.Marshal(map[string]any{"at": s.o.Now(), "entry_id": e.ID, "date": e.Date, "items": its, "totals": totals})
	if err != nil {
		return
	}
	f, err := openAppend(filepath.Join(s.o.StateDir, "coach-events.jsonl"))
	if err != nil {
		log.Printf("fuel: coach event for %s failed", e.ID)
		return
	}
	defer f.Close()
	if _, err := f.Write(append(b, '\n')); err != nil || f.Sync() != nil {
		log.Printf("fuel: coach event for %s failed", e.ID)
		return
	}
	s.coachSeen[e.ID] = true
}

// replayLog answers a repeated client_id: the stored final response, else
// the entry's CURRENT state in the same LogResponse schema (latency zero:
// nothing was processed for the repeat).
func (s *Service) replayLog(ctx context.Context, w http.ResponseWriter, rec idemRec) {
	if len(rec.Response) > 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(rec.Status)
		_, _ = w.Write(append(append([]byte(nil), rec.Response...), '\n'))
		return
	}
	e, ok := s.journal.Entry(rec.EntryID)
	if !ok {
		writeErr(w, errf(http.StatusServiceUnavailable, "busy", true, "the first request is still being written"))
		return
	}
	// Within the request's own budget: re-read a stale day (so a response
	// frozen now reflects external edits), load its history.
	ready := s.renderReady(ctx, e.Date)
	resp, status := s.buildLogResponse(e)
	if resp.renderErr != nil {
		writeErr(w, errf(http.StatusServiceUnavailable, "targets_invalid", false, "targets invalid; the request itself was saved, retry to see it"))
		return
	}
	resp.LatencyMs = map[string]int{"upload": 0, "asr": 0, "model": 0, "write": 0, "total": 0}
	switch status {
	case StatusFailed:
		writeErr(w, errf(http.StatusBadGateway, "upstream_failed", false, "Variables rejected part of the entry; nothing more is written and the saved part is being cancelled"))
	case StatusPending:
		writeJSON(w, http.StatusAccepted, resp)
	default:
		if ready {
			rec.Status = http.StatusOK
			if stored, ok := s.storeFinalOnce(rec, resp); ok {
				writeStored(w, stored)
				return
			}
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

// journalHas reports whether an entry made it into the in-memory journal.
func (s *Service) journalHas(entryID string) bool {
	_, ok := s.journal.Entry(entryID)
	return ok
}

// buildLogResponse renders an entry's current state: code-generated status
// sentence first, the model's food commentary, then the widgets.
func (s *Service) buildLogResponse(e Entry) (LogResponse, string) {
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()
	if cur, ok := s.journal.Entry(e.ID); ok {
		e = cur
	}
	snap, err := s.snapshotFor(e.Date)
	if err != nil {
		return LogResponse{renderErr: err}, ""
	}
	states := s.itemStates(e)
	var effs []Macros
	for _, st := range states {
		effs = append(effs, st.Effective)
	}
	blocks := []Block{textBlock(statusSentence(snap))}
	if e.ModelText != "" {
		blocks = append(blocks, textBlock(e.ModelText))
	}
	widgets := e.Widgets
	if e.Intent == "log" || len(widgets) == 0 {
		widgets = append([]string{"macros_today"}, widgets...)
	}
	seenW := map[string]bool{}
	for _, wn := range widgets {
		if seenW[wn] {
			continue
		}
		seenW[wn] = true
		var added map[string]*float64
		if wn == "macros_today" && e.Intent == "log" {
			added = addedOf(effs)
		}
		if b, ok := widgetBlock(wn, snap, added); ok {
			blocks = append(blocks, b)
		}
	}
	status := s.entryStatus(e)
	photos := e.PhotoIDs
	if photos == nil {
		photos = []string{}
	}
	return LogResponse{Status: status, EntryID: e.ID, Intent: e.Intent, Transcript: e.Transcript, PhotoIDs: photos,
		Items: states, Blocks: blocks, Snapshot: snap}, status
}

// appendEntryFeed writes the user line and the reply line of an entry, each
// once (keyed, so a restart can add either line a crash lost).
func (s *Service) appendEntryFeed(e Entry, blocks []Block) {
	eid := e.ID
	if !s.feed.HasKey("u:" + e.ID) {
		userText := e.UserText
		if _, err := s.feed.Append(FeedItem{At: e.CreatedAt, Role: "user", Text: &userText, PhotoIDs: e.PhotoIDs, EntryID: &eid, Key: "u:" + e.ID}); err != nil {
			log.Printf("fuel: feed append for %s failed", e.ID)
		}
	}
	if !s.feed.HasKey("r:" + e.ID) {
		if _, err := s.feed.Append(FeedItem{At: s.o.Now(), Role: "fuel", EntryID: &eid, Blocks: blocks, ShowItems: e.Intent == "log", Key: "r:" + e.ID}); err != nil {
			log.Printf("fuel: feed append for %s failed", e.ID)
		}
	}
}

func (s *Service) transcribe(ctx context.Context, audio []byte, ext string) (string, *apiError) {
	if s.o.ASR == nil {
		return "", errf(http.StatusServiceUnavailable, "busy", false, "voice transcription is not configured")
	}
	dir := filepath.Join(s.o.StateDir, "tmp")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", errf(http.StatusInternalServerError, "internal", true, "tmp dir")
	}
	f, err := os.CreateTemp(dir, "audio-*"+ext)
	if err != nil {
		return "", errf(http.StatusInternalServerError, "internal", true, "tmp file")
	}
	path := f.Name()
	defer os.Remove(path)
	_, werr := f.Write(audio)
	f.Close()
	if werr != nil {
		return "", errf(http.StatusInternalServerError, "internal", true, "tmp write")
	}
	actx, cancel := context.WithTimeout(ctx, s.o.ASRTimeout)
	defer cancel()
	tr, err := s.o.ASR.Transcribe(actx, path)
	if err != nil && ctx.Err() != nil {
		return "", errf(http.StatusGatewayTimeout, "timeout", true, "took too long; nothing was written, retry")
	}
	if err != nil {
		log.Printf("fuel: transcription failed (%s)", errClass(err))
		return "", errf(http.StatusBadGateway, "upstream_failed", true, "transcription failed")
	}
	return strings.TrimSpace(tr.Text), nil
}

func (s *Service) callModel(ctx context.Context, mi ModelInput) (*ModelOutput, *apiError) {
	for attempt := 0; attempt < 2; attempt++ {
		mctx, cancel := context.WithTimeout(ctx, s.o.ModelTimeout)
		raw, err := s.o.Model.Estimate(mctx, mi)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return nil, errf(http.StatusGatewayTimeout, "timeout", true, "took too long; nothing was written, retry")
			}
			log.Printf("fuel: model call failed (%s)", errClass(err))
			return nil, errf(http.StatusBadGateway, "upstream_failed", true, "the model call failed")
		}
		out, verr := validateOutput(raw)
		if verr == nil {
			return out, nil
		}
		log.Printf("fuel: model output invalid (attempt %d): %v", attempt+1, verr)
	}
	return nil, errf(http.StatusBadGateway, "model_invalid", true, "the model returned invalid output twice")
}

// buildItems turns model items into stored items: staple overrides, net
// carbs normalized, rounded once.
func (s *Service) buildItems(out *ModelOutput, e Entry) []Item {
	var items []Item
	for _, mi := range out.Items {
		it := Item{ID: newID("it_"), EntryID: e.ID, Date: e.Date, Name: strings.TrimSpace(mi.Item),
			PortionG: mi.PortionG, Basis: mi.PortionBasis, NeedsFraction: mi.NeedsFraction, EatenAt: e.EatenAt}
		// net_carbs_g is derived from the UNROUNDED carbs and fibre, then every
		// field is rounded once.
		net := mi.NetCarbsG
		if net == nil && mi.CarbsG != nil {
			v := *mi.CarbsG
			if mi.FiberG != nil {
				v = math.Max(0, v-*mi.FiberG)
			}
			net = &v
		}
		m := Macros{Kcal: tenthFromPtr(mi.Kcal), Protein: tenthFromPtr(mi.ProteinG), Carbs: tenthFromPtr(mi.CarbsG),
			NetCarbs: tenthFromPtr(net), Fat: tenthFromPtr(mi.FatG), SatFat: tenthFromPtr(mi.SatFatG), Fiber: tenthFromPtr(mi.FiberG)}
		if mi.StapleKey != nil {
			for _, st := range s.staples {
				if st.Key == *mi.StapleKey {
					g := st.DefaultG
					if mi.PortionG != nil {
						g = *mi.PortionG
					}
					it.PortionG = &g
					it.StapleKey = st.Key
					m = st.macros(g)
					break
				}
			}
		}
		m.normalizeNetCarbs()
		it.Orig = m
		items = append(items, it)
	}
	return items
}

// runOps posts the ops concurrently and waits for them or the budget. Work
// that outlives the budget continues durably; the status is then pending.
func (s *Service) runOps(ctx context.Context, ops []Op) string {
	done := make(chan struct{})
	go func() {
		var wg sync.WaitGroup
		for _, op := range ops {
			wg.Add(1)
			go func(op Op) {
				defer wg.Done()
				s.postOp(context.Background(), op, true)
			}(op)
		}
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
	failed, pending := false, false
	for _, op := range ops {
		cur, _ := s.journal.Op(op.ID)
		switch {
		case cur.State == OpFailed:
			failed = true
		case !terminal(cur.State):
			pending = true
		}
	}
	switch {
	case failed:
		return StatusFailed
	case pending:
		return StatusPending
	}
	return StatusDone
}

// requiredKnown fills a null in a field the Food log schema requires with 0
// (only fiber_g may be null on a row); a null there would be rejected.
func requiredKnown(m Macros) Macros {
	for _, f := range []*tenth{&m.Kcal, &m.Protein, &m.Carbs, &m.Fat, &m.SatFat} {
		if !f.OK {
			*f = known(0)
		}
	}
	return m
}

// ---- undo / fraction ----

// MutationResponse is the body of POST /fuel/undo and /fuel/fraction.
type MutationResponse struct {
	Status    string    `json:"status"`
	EntryID   string    `json:"entry_id"`
	Item      ItemState `json:"item"`
	Blocks    []Block   `json:"blocks"`
	Snapshot  Snapshot  `json:"snapshot"`
	Date      string    `json:"date"`
	renderErr error
}

var allowedFractions = []float64{0.25, 0.5, 0.75, 1}

func (s *Service) handleMutation(w http.ResponseWriter, r *http.Request, kind string) {
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(s.o.ReadDeadline))
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	var body struct {
		ClientID string   `json:"client_id"`
		ItemID   string   `json:"item_id"`
		Fraction *float64 `json:"fraction"`
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
	if !clientIDRe.MatchString(body.ClientID) || body.ItemID == "" {
		writeErr(w, errf(http.StatusBadRequest, "bad_input", false, "client_id and item_id are required"))
		return
	}
	var f float64
	if kind == "fraction" {
		ok := false
		if body.Fraction != nil {
			for _, a := range allowedFractions {
				ok = ok || *body.Fraction == a
			}
		}
		if !ok {
			writeErr(w, errf(http.StatusBadRequest, "bad_input", false, "fraction must be 0.25, 0.5, 0.75 or 1"))
			return
		}
		f = *body.Fraction
	} else if body.Fraction != nil {
		writeErr(w, errf(http.StatusBadRequest, "bad_input", false, "undo takes no fraction"))
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), s.o.Budget)
	defer cancel()
	hash := fmt.Sprintf("%s|%s|%g", kind, body.ItemID, f)
	claim, ce := s.claim(ctx, body.ClientID, kind, hash)
	if ce != nil {
		writeErr(w, ce)
		return
	}
	if claim.replay != nil {
		s.replayMutation(ctx, w, *claim.replay)
		return
	}
	defer claim.release()

	it, ok := s.journal.Item(body.ItemID)
	if !ok {
		writeErr(w, errf(http.StatusNotFound, "not_found", false, "unknown item"))
		return
	}
	l := s.itemLock(it.ID)
	if !l.LockCtx(ctx) {
		writeErr(w, errf(http.StatusGatewayTimeout, "timeout", true, "took too long; nothing was written, retry"))
		return
	}
	v := s.viewItem(it)
	var ae *apiError
	switch {
	case v.pending:
		ae = errf(http.StatusConflict, "pending", true, "the item is still being saved")
	case v.failed:
		ae = errf(http.StatusConflict, "failed", false, "the entry could not be saved")
	case !v.origDone:
		ae = errf(http.StatusConflict, "pending", true, "the item is not saved")
	case v.state.Undone:
		ae = errf(http.StatusConflict, "already_undone", false, "the item is already undone")
	case kind == "fraction" && !it.NeedsFraction:
		ae = errf(http.StatusConflict, "fraction_not_allowed", false, "this item takes no fraction")
	case kind == "fraction" && v.state.Fraction != nil:
		ae = errf(http.StatusConflict, "fraction_already_set", false, "the fraction is already set")
	}
	if ae != nil {
		l.Unlock()
		writeErr(w, ae)
		return
	}
	if ctx.Err() != nil {
		l.Unlock()
		writeErr(w, errf(http.StatusGatewayTimeout, "timeout", true, "took too long; nothing was written, retry"))
		return
	}
	now := s.o.Now()
	// ONE txn: the correction op (if any) and the fraction choice, both
	// carrying the request identity, so neither can exist without the other.
	var op *Op
	var choice *FractionChoice
	// Corrections are computed from the AUTHORITATIVE rows (re-read under
	// the item lock): external edits and deletions win (spec 14 [C8]).
	if err := s.cache.RefreshDay(ctx, it.Date); err != nil {
		l.Unlock()
		if ctx.Err() != nil {
			writeErr(w, errf(http.StatusGatewayTimeout, "timeout", true, "took too long; nothing was written, retry"))
			return
		}
		log.Printf("fuel: %s %s: read failed (%s)", kind, it.ID, errClass(err))
		writeErr(w, errf(http.StatusBadGateway, "upstream_failed", true, "could not read the current rows; nothing was written, retry"))
		return
	}
	dayRows, _, _ := s.cache.Rows(it.Date)
	ic, origRow, _ := itemContrib(dayRows, it.ID)
	if kind == "undo" {
		// Cancels the known sum of every field; deleted outside = zeros.
		o := s.newCorrectionOp(it, requiredKnown(ic.cancel()), "undo", nil)
		op = &o
	} else {
		choice = &FractionChoice{ItemID: it.ID, F: f, ClientID: body.ClientID, Hash: hash, At: now}
		// f < 1 corrects the authoritative original; if that row was deleted
		// outside, the choice is recorded without a row (nothing to scale).
		if f < 1 && origRow.Data != nil {
			base := macrosFromData(origRow.Data)
			base.normalizeNetCarbs()
			fc := f
			o := s.newCorrectionOp(it, requiredKnown(base.Scale(-(1 - f))), "fraction", &fc)
			op = &o
		}
	}
	rec := journalRec{T: "txn", Fraction: choice, At: now}
	if op != nil {
		op.ClientID, op.ReqHash = body.ClientID, hash
		op.Attempts, op.LastTry = 1, now
		rec.Ops = []Op{*op}
	}
	// Published under the exclusive render lock: a render never sees the
	// item's new pending op or fraction choice without the rest.
	s.stateMu.Lock()
	err = s.journal.AppendCtx(ctx, rec)
	s.stateMu.Unlock()
	if err != nil {
		l.Unlock()
		switch {
		case errors.Is(err, ErrDeadline):
			writeErr(w, errf(http.StatusGatewayTimeout, "timeout", true, "took too long; nothing was written, retry"))
		case errors.Is(err, ErrJournalBroken):
			e := errf(http.StatusServiceUnavailable, "busy", true, "storage problem on the server; retry later")
			e.retryAfter = 60
			writeErr(w, e)
		default:
			writeErr(w, errf(http.StatusInternalServerError, "internal", true, "could not persist the request; nothing was written"))
		}
		return
	}
	idemBase := idemRec{ClientID: body.ClientID, Hash: hash, Kind: kind, At: now, EntryID: it.EntryID, ItemID: it.ID}
	if err := s.idem.Put(idemBase); err != nil {
		log.Printf("fuel: idem reservation for %s failed (the journal still holds it)", it.ID)
	}
	l.Unlock()

	if op != nil {
		s.runOps(ctx, []Op{*op})
	}
	ready := s.renderReady(ctx, it.Date)
	resp := s.mutationResponse(it, kind, f, op) // status captured in the render
	if resp.renderErr != nil {
		writeErr(w, errf(http.StatusServiceUnavailable, "targets_invalid", false, "targets invalid; the request itself was saved, retry to see it"))
		return
	}
	status := resp.Status
	if status == StatusFailed {
		writeErr(w, errf(http.StatusBadGateway, "upstream_failed", false, "Variables rejected the correction"))
		return
	}
	code := http.StatusOK
	if status == StatusPending {
		code = http.StatusAccepted
	}
	b, _ := json.Marshal(resp)
	if status == StatusDone && ready {
		rec := idemBase
		rec.Status, rec.Response = code, b
		_ = s.idem.Put(rec)
	}
	if ready {
		s.appendMutationFeed(mutationKey(body.ClientID, s.journal), it.EntryID, resp.Blocks)
	}
	log.Printf("fuel: %s item=%s status=%s", kind, it.ID, status)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(append(b, '\n'))
}

func (s *Service) mutationResponse(it Item, kind string, f float64, op *Op) MutationResponse {
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()
	return s.mutationResponseLocked(it, kind, f, op)
}

func (s *Service) mutationResponseLocked(it Item, kind string, f float64, op *Op) MutationResponse {
	snap, err := s.snapshotFor(it.Date)
	if err != nil {
		return MutationResponse{renderErr: err}
	}
	v := s.viewItem(it)
	var added map[string]*float64
	if op != nil {
		added = addedOf([]Macros{op.Macros})
	} else {
		added = addedOf(nil)
	}
	var lead string
	if kind == "undo" {
		lead = "Removed " + it.Name + "."
	} else {
		lead = fmt.Sprintf("Counted %s of %s.", fractionWords(f), it.Name)
	}
	blocks := []Block{textBlock(lead + " " + statusSentence(snap))}
	if b, ok := widgetBlock("macros_today", snap, added); ok {
		blocks = append(blocks, b)
	}
	status := StatusDone
	if op != nil {
		if o, ok := s.journal.Op(op.ID); ok {
			switch {
			case o.State == OpFailed:
				status = StatusFailed
			case !terminal(o.State):
				status = StatusPending
			}
		}
	}
	return MutationResponse{Status: status, EntryID: it.EntryID, Item: v.state, Blocks: blocks, Snapshot: snap, Date: it.Date}
}

func fractionWords(f float64) string {
	switch f {
	case 0.25:
		return "a quarter"
	case 0.5:
		return "half"
	case 0.75:
		return "three quarters"
	}
	return "all"
}

func (s *Service) replayMutation(ctx context.Context, w http.ResponseWriter, rec idemRec) {
	if len(rec.Response) > 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(rec.Status)
		_, _ = w.Write(append(append([]byte(nil), rec.Response...), '\n'))
		return
	}
	ready := false
	if id, ok := s.journal.Ident(rec.ClientID); ok {
		if it, ok := s.journal.Item(id.ItemID); ok {
			ready = s.renderReady(ctx, it.Date)
		}
	}
	resp, found := s.rebuildMutation(rec.ClientID)
	switch {
	case !found:
		writeErr(w, errf(http.StatusServiceUnavailable, "busy", true, "the first request is still being written"))
		return
	case resp.renderErr != nil:
		writeErr(w, errf(http.StatusServiceUnavailable, "targets_invalid", false, "targets invalid; the request itself was saved, retry to see it"))
		return
	}
	switch resp.Status {
	case StatusFailed:
		writeErr(w, errf(http.StatusBadGateway, "upstream_failed", false, "Variables rejected the correction"))
	case StatusPending:
		writeJSON(w, http.StatusAccepted, resp)
	default:
		// The first complete done render becomes THE final response; if one
		// was stored meanwhile, that one is answered.
		if ready {
			rec.Status = http.StatusOK
			if stored, ok := s.storeFinalOnce(rec, resp); ok {
				writeStored(w, stored)
				return
			}
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

// writeStored answers with a stored final response.
func writeStored(w http.ResponseWriter, rec idemRec) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(rec.Status)
	_, _ = w.Write(append(append([]byte(nil), rec.Response...), '\n'))
}

// rebuildMutation renders a journaled undo / fraction request's CURRENT
// state in the MutationResponse schema.
func (s *Service) rebuildMutation(clientID string) (MutationResponse, bool) {
	id, ok := s.journal.Ident(clientID)
	if !ok {
		return MutationResponse{}, false
	}
	it, ok := s.journal.Item(id.ItemID)
	if !ok {
		return MutationResponse{}, false
	}
	var op *Op
	if id.OpID != "" {
		if o, ok := s.journal.Op(id.OpID); ok {
			op = &o
		}
	}
	f := 1.0
	if op != nil && op.Fraction != nil {
		f = *op.Fraction
	} else if v, ok := s.journal.Fraction(it.ID); ok && id.Kind == "fraction" {
		f = v
	}
	resp := s.mutationResponse(it, id.Kind, f, op)
	return resp, true // the caller checks resp.renderErr
}

// appendMutationFeed writes the reply line of an undo / fraction once per
// request generation (mutationKey), so a restart can add a line a crash lost.
func (s *Service) appendMutationFeed(key, entryID string, blocks []Block) {
	if s.feed.HasKey(key) {
		return
	}
	txt := blocks[0].Text
	eid := entryID
	if _, err := s.feed.Append(FeedItem{At: s.o.Now(), Role: "fuel", Text: &txt, EntryID: &eid, Blocks: blocks, Key: key}); err != nil {
		log.Printf("fuel: feed append failed")
	}
}

// ---- GET routes ----

type entryResponse struct {
	Status   string      `json:"status"`
	EntryID  string      `json:"entry_id"`
	Items    []ItemState `json:"items"`
	Blocks   []Block     `json:"blocks"`
	Snapshot Snapshot    `json:"snapshot"`
}

func (s *Service) writeEntry(w http.ResponseWriter, e Entry) {
	s.stateMu.RLock()
	if cur, ok := s.journal.Entry(e.ID); ok {
		e = cur
	}
	snap, err := s.snapshotFor(e.Date)
	if err != nil {
		s.stateMu.RUnlock()
		writeErr(w, errf(http.StatusServiceUnavailable, "targets_invalid", false, "targets invalid"))
		return
	}
	states := s.itemStates(e)
	var effs []Macros
	for _, st := range states {
		effs = append(effs, st.Effective)
	}
	blocks := []Block{textBlock(statusSentence(snap))}
	if b, ok := widgetBlock("macros_today", snap, addedOf(effs)); ok {
		blocks = append(blocks, b)
	}
	status := s.entryStatus(e)
	s.stateMu.RUnlock()
	code := http.StatusOK
	if status == StatusPending {
		code = http.StatusAccepted
	}
	writeJSON(w, code, entryResponse{Status: status, EntryID: e.ID, Items: states, Blocks: blocks, Snapshot: snap})
}

func (s *Service) handleEntry(w http.ResponseWriter, r *http.Request) {
	e, ok := s.journal.Entry(r.PathValue("id"))
	if !ok {
		writeErr(w, errf(http.StatusNotFound, "not_found", false, "unknown entry"))
		return
	}
	s.freshen(r.Context(), e.Date)
	s.ensureHistory(r.Context(), e.Date)
	s.writeEntry(w, e)
}

func (s *Service) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	today := s.today()
	date := r.URL.Query().Get("date")
	if date == "" {
		date = today
	}
	if _, err := time.Parse("2006-01-02", date); err != nil || date > today || date < dateAdd(today, -34) {
		writeErr(w, errf(http.StatusBadRequest, "bad_input", false, "date must be today or one of the 34 days before it"))
		return
	}
	s.freshen(r.Context(), date)
	s.ensureHistory(r.Context(), date)
	snap, err := s.snapshotFor(date)
	if err != nil {
		writeErr(w, errf(http.StatusServiceUnavailable, "targets_invalid", false, "targets invalid"))
		return
	}
	writeJSON(w, http.StatusOK, snap)
}

type feedOut struct {
	ID       string      `json:"id"`
	At       time.Time   `json:"at"`
	Role     string      `json:"role"`
	Text     *string     `json:"text"`
	PhotoIDs []string    `json:"photo_ids"`
	EntryID  *string     `json:"entry_id"`
	Items    []ItemState `json:"items"`
	Blocks   []Block     `json:"blocks"`
}

func (s *Service) handleFeed(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := 50
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 100 {
			writeErr(w, errf(http.StatusBadRequest, "bad_input", false, "limit must be 1..100"))
			return
		}
		limit = n
	}
	var before int64
	if v := q.Get("before"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 1 {
			writeErr(w, errf(http.StatusBadRequest, "bad_input", false, "bad cursor"))
			return
		}
		before = n
	}
	page, next := s.feed.Page(before, limit)
	// Item state in the feed is CURRENT: re-read every stale day behind this
	// page's item cards (4 at a time; fresh days are skipped by freshen),
	// however old: the feed is not bound to the snapshot's 35-day window.
	stale := map[string]bool{}
	for i := len(page) - 1; i >= 0; i-- {
		if page[i].ShowItems && page[i].EntryID != nil {
			if e, ok := s.journal.Entry(*page[i].EntryID); ok {
				stale[e.Date] = true
			}
		}
	}
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for d := range stale {
		wg.Add(1)
		sem <- struct{}{}
		go func(d string) {
			defer wg.Done()
			defer func() { <-sem }()
			s.freshen(r.Context(), d)
		}(d)
	}
	wg.Wait()
	// Collect every card under the render lock, after the refreshes.
	s.stateMu.RLock()
	out := make([]feedOut, 0, len(page))
	for _, it := range page {
		fo := feedOut{ID: it.ID, At: it.At, Role: it.Role, Text: it.Text, PhotoIDs: it.PhotoIDs, EntryID: it.EntryID, Items: []ItemState{}, Blocks: it.Blocks}
		if it.ShowItems && it.EntryID != nil {
			if e, ok := s.journal.Entry(*it.EntryID); ok {
				fo.Items = s.itemStates(e) // CURRENT state
			}
		}
		if fo.PhotoIDs == nil {
			fo.PhotoIDs = []string{}
		}
		if fo.Blocks == nil {
			fo.Blocks = []Block{}
		}
		out = append(out, fo)
	}
	s.stateMu.RUnlock() // never hold the render lock across a network write
	var nb *string
	if next != "" {
		nb = &next
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out, "next_before": nb})
}

func (s *Service) handlePhoto(w http.ResponseWriter, r *http.Request) {
	p := s.photos.path(r.PathValue("id"))
	if p == "" {
		writeErr(w, errf(http.StatusNotFound, "not_found", false, "photo not found or pruned"))
		return
	}
	b, err := os.ReadFile(p)
	if err != nil {
		writeErr(w, errf(http.StatusNotFound, "not_found", false, "photo not found"))
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, bytes.NewReader(b))
}

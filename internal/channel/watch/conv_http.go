package watch

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// handleConversations serves GET /watch/conversations?limit=&before=&archived=0|1:
// {"conversations": [...], "next": "<cursor>" | null}.
func (a *Adapter) handleConversations(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	archived := false
	switch q.Get("archived") {
	case "", "0", "false":
	case "1", "true":
		archived = true
	default:
		writeErr(w, http.StatusBadRequest, "bad archived")
		return
	}
	rows, next, err := a.store.Conversations(archived, q.Get("before"), limit)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if rows == nil {
		rows = []ConvInfo{}
	}
	var nextV any
	if next != "" {
		nextV = next
	}
	writeJSON(w, http.StatusOK, map[string]any{"conversations": rows, "next": nextV})
}

// handleConversation serves PUT (upsert {title?}) and PATCH ({title?,
// archived?}) on /watch/conversations/<id>. main answers 400, a bad id 400,
// an unknown id on PATCH 404.
func (a *Adapter) handleConversation(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/watch/conversations/")
	var create bool
	switch r.Method {
	case http.MethodPut:
		create = true
	case http.MethodPatch:
	case http.MethodGet:
		// A single row (convenience for the app; not in the contract's
		// required set).
		if id != ConvMain && !ValidConvID(id) {
			writeErr(w, http.StatusBadRequest, "bad conversation id")
			return
		}
		info, ok := a.store.ConvInfo(id)
		if !ok {
			writeErr(w, http.StatusNotFound, "unknown conversation")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"conversation": info})
		return
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if id == ConvMain {
		writeErr(w, http.StatusBadRequest, "main cannot be renamed or archived")
		return
	}
	if !ValidConvID(id) {
		writeErr(w, http.StatusBadRequest, "bad conversation id")
		return
	}
	var body struct {
		Title    *string `json:"title"`
		Archived *bool   `json:"archived"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&body); err != nil && err != io.EOF {
		writeErr(w, http.StatusBadRequest, "bad JSON body")
		return
	}
	if create && body.Archived != nil {
		writeErr(w, http.StatusBadRequest, "PUT takes a title only; use PATCH to archive")
		return
	}
	info, err := a.store.UpsertConv(id, ConvPatch{Title: body.Title, Archived: body.Archived}, create)
	switch {
	case errors.Is(err, ErrUnknownConv):
		writeErr(w, http.StatusNotFound, "unknown conversation")
		return
	case errors.Is(err, ErrMainConv):
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	case err != nil:
		writeErr(w, http.StatusServiceUnavailable, "could not store the conversation, try again")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"conversation": info})
}

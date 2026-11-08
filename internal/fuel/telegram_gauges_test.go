package fuel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Exercise the actual multipart request, not just the renderer or an API mock.
func TestTelegramGaugesReplies(t *testing.T) {
	for _, tc := range []struct {
		name, status                                                   string
		delta                                                          *float64
		question, disabled, yesterday, pending, renderFail, uploadFail bool
		wantPhotos                                                     int
	}{
		{name: "log", status: StatusDone, delta: fptr(27), wantPhotos: 1},
		{name: "correction", status: StatusDone, delta: fptr(-10), wantPhotos: 1},
		{name: "pending completes", status: StatusPending, pending: true, delta: fptr(27), wantPhotos: 1},
		{name: "backdated", status: StatusDone, yesterday: true, delta: fptr(27), wantPhotos: 1},
		{name: "question", status: StatusDone, question: true},
		{name: "failed", status: StatusFailed, delta: fptr(27)},
		{name: "disabled", status: StatusDone, disabled: true, delta: fptr(27)},
		{name: "render failure", status: StatusDone, renderFail: true, delta: fptr(27)},
		{name: "upload failure", status: StatusDone, uploadFail: true, delta: fptr(27), wantPhotos: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snap := gaugeFixture()
			widgetSnap := snap
			if tc.yesterday {
				widgetSnap.Date = "2026-11-07"
			}
			added := map[string]*float64{"protein_g": tc.delta}
			if tc.question {
				added = nil
			}
			widget, _ := widgetBlock("macros_today", widgetSnap, added)
			blocks := []Block{textBlock("Logged: skyr 250 g."), widget}
			if tc.renderFail {
				snap.Budgets = nil
			}
			photos, texts, snapshotReads, entryReads := 0, 0, 0, 0
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "POST" {
					t.Errorf("method: %s", r.Method)
				}
				switch r.URL.Path {
				case "/bot" + tgTok + "/sendChatAction":
				case "/bot" + tgTok + "/sendMessage":
					texts++
				case "/bot" + tgTok + "/sendPhoto":
					photos++
					if err := r.ParseMultipartForm(2 << 20); err != nil {
						t.Error(err)
						return
					}
					defer r.MultipartForm.RemoveAll()
					if r.FormValue("chat_id") != fmt.Sprint(tgJoe) || r.FormValue("parse_mode") != "" {
						t.Errorf("photo fields: %v", r.MultipartForm.Value)
					}
					if caption := r.FormValue("caption"); caption != "Logged: skyr 250 g. 87 / 160 g protein" || strings.ContainsAny(caption, "\r\n") {
						t.Errorf("caption: %q", caption)
					}
					var rp struct {
						MessageID int64 `json:"message_id"`
						Allow     bool  `json:"allow_sending_without_reply"`
					}
					if err := json.Unmarshal([]byte(r.FormValue("reply_parameters")), &rp); err != nil || rp.MessageID != 501 || !rp.Allow {
						t.Errorf("reply parameters: %+v, %v", rp, err)
					}
					file, header, err := r.FormFile("photo")
					if err != nil {
						t.Error(err)
						return
					}
					defer file.Close()
					if header.Filename != "gauges.png" || header.Header.Get("Content-Type") != "image/png" {
						t.Errorf("photo header: %+v", header)
					}
					raw, _ := io.ReadAll(file)
					wantAdded := added
					if tc.yesterday {
						wantAdded = nil
					}
					want, err := RenderGauges(snap, wantAdded)
					if err != nil || !bytes.Equal(raw, want) {
						t.Errorf("photo differs from chart: %v", err)
					}
					if tc.uploadFail {
						w.WriteHeader(http.StatusBadGateway)
						_, _ = w.Write([]byte(`{"ok":false,"error_code":502}`))
						return
					}
				default:
					t.Errorf("unexpected API path: %s", r.URL.Path)
				}
				_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":600}}`))
			}))
			defer api.Close()
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer "+testToken {
					t.Error("snapshot/entry read lacks app auth")
				}
				switch r.URL.Path {
				case "/fuel/snapshot":
					snapshotReads++
					_ = json.NewEncoder(w).Encode(snap)
				case "/fuel/entry/en_test":
					entryReads++
					_ = json.NewEncoder(w).Encode(entryResponse{Status: StatusDone, EntryID: "en_test", Blocks: blocks})
				default:
					t.Errorf("unexpected fuel path: %s", r.URL.Path)
				}
			})
			b := NewTelegramBot(TelegramOptions{Token: tgTok, API: api.URL, AppToken: testToken, Handler: handler,
				DisableGauges: tc.disabled, Client: loopbackClient()})
			body, _ := json.Marshal(LogResponse{Status: tc.status, EntryID: "en_test", Blocks: blocks})
			b.finish(context.Background(), tgJoe, 501, http.StatusOK, body)
			if photos != tc.wantPhotos || texts != 1 {
				t.Fatalf("photos %d (want %d), text replies %d (want 1)", photos, tc.wantPhotos, texts)
			}
			if (tc.disabled || tc.question || tc.status == StatusFailed) && snapshotReads != 0 {
				t.Errorf("unexpected snapshot read: %d", snapshotReads)
			}
			if tc.pending && entryReads != 1 {
				t.Errorf("pending entry reads: %d", entryReads)
			}
		})
	}
}

func TestTelegramGaugesCommand(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("disabled=%t", disabled), func(t *testing.T) {
			photos, texts, reads := 0, 0, 0
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/bot" + tgTok + "/sendPhoto":
					photos++
				case "/bot" + tgTok + "/sendMessage":
					texts++
				default:
					t.Errorf("unexpected call: %s", r.URL.Path)
				}
				_, _ = w.Write([]byte(`{"ok":true,"result":{}}`))
			}))
			defer api.Close()
			h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" || r.URL.Path != "/fuel/snapshot" {
					t.Errorf("command wrote or called model: %s %s", r.Method, r.URL.Path)
				}
				reads++
				_ = json.NewEncoder(w).Encode(gaugeFixture())
			})
			b := NewTelegramBot(TelegramOptions{Token: tgTok, API: api.URL, AppToken: testToken, Handler: h,
				DisableGauges: disabled, Client: loopbackClient()})
			m := &tgMessage{Text: "/gauges", MessageID: 42, Date: time.Now().Unix()}
			m.Chat.ID = tgJoe
			if !b.handle(context.Background(), []*tgMessage{m}) {
				t.Fatal("command was not handled")
			}
			b.replies.Wait()
			if disabled {
				if photos != 0 || reads != 0 || texts != 1 {
					t.Fatalf("disabled: photos %d, reads %d, texts %d", photos, reads, texts)
				}
			} else if photos != 1 || reads != 1 || texts != 0 {
				t.Fatalf("command: photos %d, reads %d, texts %d", photos, reads, texts)
			}
		})
	}
}

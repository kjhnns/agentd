package watch

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
)

// TestSinceCursorNeverSkipsAChange is the D3 regression: Since scanned the
// log by seq but returned each id's LATEST snapshot and the global
// high-water seq as the cursor. With A@2, B@3, A@4 and limit 1 it returned
// A@4 with cursor 4, and a client that advanced to it never saw B.
func TestSinceCursorNeverSkipsAChange(t *testing.T) {
	st, _ := OpenStore("")
	x := st.Append(Message{Role: RoleSystem, Kind: KindNotice, Text: "x"})
	a := st.Append(Message{Role: RoleUser, Kind: KindText, Text: "a", Status: StatusQueued})
	b := st.Append(Message{Role: RoleUser, Kind: KindText, Text: "b", Status: StatusQueued})
	st.Update(a.ID, func(m *Message) bool { m.Status = StatusWorking; return true })

	seen := map[string]bool{}
	cursor := x.Seq
	for i := 0; i < 10; i++ {
		msgs, next, reset := st.Since(cursor, 1)
		if reset {
			t.Fatal("unexpected reset")
		}
		if len(msgs) == 0 {
			break
		}
		for _, m := range msgs {
			seen[m.ID] = true
		}
		if next <= cursor {
			t.Fatalf("cursor did not advance: %d -> %d", cursor, next)
		}
		cursor = next
	}
	if !seen[a.ID] || !seen[b.ID] {
		t.Fatalf("paging with limit 1 skipped a change: saw a=%v b=%v", seen[a.ID], seen[b.ID])
	}
}

// TestFetchReportsHasMoreChanges: the HTTP shape keeps latest_seq (now the
// cursor) and adds has_more_changes.
func TestFetchReportsHasMoreChanges(t *testing.T) {
	ad, st := newAdapter(t)
	x := st.Append(Message{Role: RoleSystem, Kind: KindNotice, Text: "x"})
	st.Append(Message{Role: RoleSystem, Kind: KindNotice, Text: "y"})
	z := st.Append(Message{Role: RoleSystem, Kind: KindNotice, Text: "z"})
	rr := do(t, ad.Handler(), http.MethodGet, "/watch/messages?after="+strconv.FormatInt(x.Seq, 10)+"&limit=1", "wt", nil, "")
	var body struct {
		Messages       []Message `json:"messages"`
		LatestSeq      int64     `json:"latest_seq"`
		HasMoreChanges *bool     `json:"has_more_changes"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.HasMoreChanges == nil || !*body.HasMoreChanges || len(body.Messages) != 1 || body.LatestSeq != x.Seq+1 {
		t.Fatalf("page = %s", rr.Body.String())
	}
	rr = do(t, ad.Handler(), http.MethodGet, "/watch/messages?after="+strconv.FormatInt(body.LatestSeq, 10)+"&limit=5", "wt", nil, "")
	body.HasMoreChanges = nil
	_ = json.Unmarshal(rr.Body.Bytes(), &body)
	if body.HasMoreChanges == nil || *body.HasMoreChanges || body.LatestSeq != z.Seq {
		t.Fatalf("last page = %s", rr.Body.String())
	}
}

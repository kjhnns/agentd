package watch

import (
	"bufio"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func archiveLines(t *testing.T, dir, conv string) []string {
	t.Helper()
	f, err := os.Open(filepath.Join(dir, "watch-archive", conv+".jsonl"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		out = append(out, sc.Text())
	}
	return out
}

// TestArchiveBeforeTrimOutOfOrderAndReplayTwice: eviction runs in insertion
// order, not seq order. A message whose status changed late (a high seq) is
// evicted before an older-seq one; both reach the archive exactly once, and
// two more opens (replays that trim again) add nothing.
func TestArchiveBeforeTrimOutOfOrderAndReplayTwice(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "watch-messages.jsonl")
	st, err := openStoreWith(path, 1<<20, 3)
	if err != nil {
		t.Fatal(err)
	}
	first := st.Append(Message{Role: RoleUser, Kind: KindText, Text: "m1", Status: StatusDone, Conv: "a"})
	second := st.Append(Message{Role: RoleUser, Kind: KindText, Text: "m2", Status: StatusDone, Conv: "a"})
	st.Append(Message{Role: RoleUser, Kind: KindText, Text: "m3", Status: StatusDone, Conv: "a"})
	// m1 changes last: its seq is now the highest of the three.
	if _, ok := st.Update(first.ID, func(m *Message) bool { m.Text = "m1 edited"; return true }); !ok {
		t.Fatal("update")
	}
	st.Append(Message{Role: RoleUser, Kind: KindText, Text: "m4", Status: StatusDone, Conv: "a"})
	st.Append(Message{Role: RoleUser, Kind: KindText, Text: "m5", Status: StatusDone, Conv: "a"})
	st.Close()

	lines := archiveLines(t, dir, "a")
	if len(lines) != 2 || !strings.Contains(lines[0], "m1 edited") || !strings.Contains(lines[1], `"`+second.ID+`"`) {
		t.Fatalf("archive = %v", lines)
	}
	if info, err := os.Stat(filepath.Join(dir, "watch-archive", "a.jsonl")); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("archive mode: %v %v", info.Mode(), err)
	}
	for i := 0; i < 2; i++ {
		st, err = openStoreWith(path, 1<<20, 3)
		if err != nil {
			t.Fatal(err)
		}
		st.Close()
	}
	if got := archiveLines(t, dir, "a"); len(got) != 2 {
		t.Fatalf("a replay archived again: %d lines", len(got))
	}
	// Compaction after the trim drops the evicted lines from the store; the
	// transcript still has all five, newest snapshot of m1.
	st, err = openStoreWith(path, 0, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	tr, err := st.Transcript("a")
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, m := range tr {
		texts = append(texts, m.Text)
	}
	if strings.Join(texts, ",") != "m1 edited,m2,m3,m4,m5" {
		t.Fatalf("transcript = %v", texts)
	}
}

// TestArchiveFailureStopsEviction: the store grows rather than loses.
func TestArchiveFailureStopsEviction(t *testing.T) {
	dir := t.TempDir()
	st, err := openStoreWith(filepath.Join(dir, "w.jsonl"), 1<<20, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	st.failArchive = func() error { return errors.New("disk full") }
	for i := 0; i < 4; i++ {
		st.Append(Message{Role: RoleAgent, Kind: KindReply, Text: "x"})
	}
	if msgs, _, _ := st.Tail(50); len(msgs) != 4 {
		t.Fatalf("evicted without an archive: %d left", len(msgs))
	}
	st.failArchive = nil
	st.Append(Message{Role: RoleAgent, Kind: KindReply, Text: "y"})
	if msgs, _, _ := st.Tail(50); len(msgs) != 2 {
		t.Fatalf("after the archive works again: %d left", len(msgs))
	}
	if n := len(archiveLines(t, dir, "main")); n != 3 {
		t.Fatalf("archived %d", n)
	}
}

// TestRetentionIsPerConversation: traffic in main never evicts a quiet side
// conversation's messages, and conversation records are never evicted.
func TestRetentionIsPerConversation(t *testing.T) {
	st, err := openStoreWith(filepath.Join(t.TempDir(), "w.jsonl"), 1<<20, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.UpsertConv("b", ConvPatch{}, true); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		st.Append(Message{Role: RoleUser, Kind: KindText, Text: "side", Status: StatusDone, Conv: "b"})
	}
	for i := 0; i < 10; i++ {
		st.Append(Message{Role: RoleUser, Kind: KindText, Text: "main", Status: StatusDone})
	}
	if msgs, _, _ := st.TailF(InConv("b"), 50); len(msgs) != 3 { // record + 2
		t.Fatalf("side thread = %d", len(msgs))
	}
	if msgs, _, _ := st.TailF(MainOnly, 50); len(msgs) != 3 {
		t.Fatalf("main = %d", len(msgs))
	}
}

// TestConvRecordsSurviveCompaction: the latest record of a quiet
// conversation survives trims and compactions with its title.
func TestConvRecordsSurviveCompaction(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "w.jsonl")
	st, err := openStoreWith(path, 1<<20, 2)
	if err != nil {
		t.Fatal(err)
	}
	title := "Train to Basel"
	if _, err := st.UpsertConv("quiet", ConvPatch{Title: &title}, true); err != nil {
		t.Fatal(err)
	}
	renamed := "Train to Basel, Friday"
	if _, err := st.UpsertConv("quiet", ConvPatch{Title: &renamed}, false); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		st.Append(Message{Role: RoleAgent, Kind: KindReply, Text: "noise"})
	}
	st.Close()
	for i := 0; i < 2; i++ {
		st, err = openStoreWith(path, 0, 2) // compacts on every open
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			st.Close()
		}
	}
	defer st.Close()
	info, ok := st.ConvInfo("quiet")
	if !ok || info.Title != renamed {
		t.Fatalf("conversation after compaction = %+v %v", info, ok)
	}
	n, _ := st.replayCount()
	if n > 6 {
		t.Fatalf("compaction kept %d lines", n)
	}
}

// TestConversationsPaging: main first on the first page, newest activity
// first, a cursor that walks every row once, archived ones apart.
func TestConversationsPaging(t *testing.T) {
	st, _ := OpenStore("")
	base := time.Date(2026, 10, 24, 10, 0, 0, 0, time.UTC)
	ids := []string{"c1", "c2", "c3", "c4", "c5"}
	for i, id := range ids {
		if _, err := st.UpsertConv(id, ConvPatch{}, true); err != nil {
			t.Fatal(err)
		}
		st.Append(Message{Role: RoleUser, Kind: KindText, Text: "hello " + id, Status: StatusDone, Conv: id, TS: base.Add(time.Duration(i) * time.Minute)})
	}
	yes := true
	if _, err := st.UpsertConv("c3", ConvPatch{Archived: &yes}, false); err != nil {
		t.Fatal(err)
	}
	var seen []string
	cursor := ""
	for page := 0; page < 5; page++ {
		rows, next, err := st.Conversations(false, cursor, 3)
		if err != nil {
			t.Fatal(err)
		}
		for i, r := range rows {
			if r.ID == ConvMain && (page != 0 || i != 0) {
				t.Fatal("main is not the first row of the first page only")
			}
			seen = append(seen, r.ID)
		}
		if next == "" {
			break
		}
		cursor = next
	}
	if got := strings.Join(seen, ","); got != "main,c5,c4,c2,c1" {
		t.Fatalf("rows = %s", got)
	}
	rows, next, _ := st.Conversations(true, "", 50)
	if len(rows) != 1 || rows[0].ID != "c3" || next != "" || !rows[0].Archived {
		t.Fatalf("archived = %+v %q", rows, next)
	}
	if rows[0].Preview != "hello c3" || rows[0].Count != 1 {
		t.Fatalf("row = %+v", rows[0])
	}
	if _, _, err := st.Conversations(false, "garbage", 3); err == nil {
		t.Fatal("a bad cursor was accepted")
	}
}

// TestUpsertConvValidation: main and bad ids are refused, an unknown id is
// created only by an upsert.
func TestUpsertConvValidation(t *testing.T) {
	st, _ := OpenStore("")
	if _, err := st.UpsertConv(ConvMain, ConvPatch{}, true); !errors.Is(err, ErrMainConv) {
		t.Fatalf("main: %v", err)
	}
	if _, err := st.UpsertConv("Bad/Id", ConvPatch{}, true); err == nil {
		t.Fatal("bad id accepted")
	}
	if _, err := st.UpsertConv("nope", ConvPatch{}, false); !errors.Is(err, ErrUnknownConv) {
		t.Fatalf("unknown: %v", err)
	}
}

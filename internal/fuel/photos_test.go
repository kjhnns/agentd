package fuel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTwoPhotosAreOneMeal(t *testing.T) {
	h := newHarness(t)
	var got ModelInput
	h.model.fn = func(in ModelInput) string {
		got = in
		return `{"intent":"log","items":[
 {"item":"pizza","kind":"food","staple_key":null,"portion_g":300,"portion_basis":"label","kcal":800,"protein_g":30,"carbs_g":100,"net_carbs_g":null,"fat_g":30,"sat_fat_g":13,"fiber_g":null,"needs_fraction":false,"volume_ml":null,"caffeine_mg":null,"alcohol_g":null},
 {"item":"salad","kind":"food","staple_key":null,"portion_g":100,"portion_basis":"photo_estimate","kcal":40,"protein_g":2,"carbs_g":5,"net_carbs_g":null,"fat_g":1,"sat_fat_g":0.2,"fiber_g":2,"needs_fraction":false,"volume_ml":null,"caffeine_mg":null,"alcohol_g":null}],"text":"","widgets":[]}`
	}
	body, ct := multipartBody(t, map[string]string{"client_id": "p0000001-0001"}, []filePart{
		{"image", "a.jpg", "image/jpeg", testJPEG(300, 200)},
		{"image", "b.jpg", "image/jpeg", testJPEG(200, 300)},
	})
	r := h.do("POST", "/fuel/log", body, ct)
	if r.Code != 200 {
		t.Fatalf("%d %s", r.Code, r.Body)
	}
	resp := decode[LogResponse](t, r)
	if len(got.Images) != 2 {
		t.Fatalf("model got %d images", len(got.Images))
	}
	sys := systemFor(got)
	if !strings.Contains(sys, "This request has 2 photos. They all show the SAME meal") || !strings.Contains(systemPrompt, "never separate servings") || !strings.Contains(systemPrompt, `portion_basis "label"`) {
		t.Fatal("same-meal rule missing from the system prompt")
	}
	if len(resp.PhotoIDs) != 2 {
		t.Fatalf("photo_ids %v", resp.PhotoIDs)
	}
	want := strings.Join(resp.PhotoIDs, ",")
	rows := h.vars.rows("var-food")
	if len(rows) != 2 {
		t.Fatalf("rows %d", len(rows))
	}
	for _, row := range rows {
		if row["photo_ref"] != want {
			t.Fatalf("row %v photo_ref %v want %s", row["item"], row["photo_ref"], want)
		}
	}
	d := h.day("")
	if len(d.Items) != 2 {
		t.Fatalf("day items %d", len(d.Items))
	}
	for _, it := range d.Items {
		var ids []string
		for _, x := range it["photo_ids"].([]any) {
			ids = append(ids, x.(string))
		}
		if strings.Join(ids, ",") != want || it["photo_id"] != resp.PhotoIDs[0] {
			t.Fatalf("day photos %v %v", ids, it["photo_id"])
		}
	}
	feed := decode[struct {
		Items []feedOut `json:"items"`
	}](t, h.do("GET", "/fuel/feed", nil, ""))
	if strings.Join(feed.Items[0].PhotoIDs, ",") != want {
		t.Fatalf("feed photo_ids %v", feed.Items[0].PhotoIDs)
	}
	e := decode[entryResponse](t, h.do("GET", "/fuel/entry/"+resp.EntryID, nil, ""))
	if strings.Join(e.PhotoIDs, ",") != want {
		t.Fatalf("entry photo_ids %v", e.PhotoIDs)
	}
	// Upload order: the first upload is 300x200, the second 200x300.
	for i, id := range resp.PhotoIDs {
		r := h.do("GET", "/fuel/photo/"+id, nil, "")
		if r.Code != 200 {
			t.Fatalf("photo %s: %d", id, r.Code)
		}
		cfg, err := jpeg.DecodeConfig(bytes.NewReader(r.Body.Bytes()))
		if err != nil {
			t.Fatal(err)
		}
		want := [][2]int{{300, 200}, {200, 300}}[i]
		if cfg.Width != want[0] || cfg.Height != want[1] {
			t.Fatalf("photo %d is %dx%d: not in upload order", i, cfg.Width, cfg.Height)
		}
	}
	// One photo: no multi-photo note.
	if strings.Contains(systemFor(ModelInput{Images: [][]byte{{1}}}), "This request has") {
		t.Fatal("single photo got the multi-photo note")
	}
}

func TestOpenAIRequestCarriesAllPhotosAndTheRule(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{}"}}]}`))
	}))
	defer srv.Close()
	o := &OpenAI{Key: "k", Model: "m", BaseURL: srv.URL, Client: loopbackClient()}
	_, _ = o.Estimate(context.Background(), ModelInput{Images: [][]byte{{1}, {2}, {3}}})
	msgs := body["messages"].([]any)
	sys := msgs[0].(map[string]any)["content"].(string)
	user, _ := json.Marshal(msgs[1])
	if strings.Count(string(user), "image_url") < 3 || !strings.Contains(sys, "This request has 3 photos") {
		t.Fatal("request lacks the photos or the rule")
	}
}

func undoNamesOut(names []string, which string) string {
	b, _ := json.Marshal(names)
	return fmt.Sprintf(`{"intent":"undo","items":[],"corrections":[],"targets":[{"ref":"last","names":%s,"which":%s,"volume_ml":null,"portion_g":null}],"text":"","widgets":[]}`, b, which)
}

func TestVagueRemovalFiltersByTheNamedFood(t *testing.T) {
	h := newHarness(t)
	h.model.fn = func(ModelInput) string { return oneItem("skyr", 250, 157.5, 27.5, false) }
	h.logText("p0000002-0001", "skyr")
	agentdRow(h, "filter coffee", 300, 2, "2026-10-01T11:58:00+02:00", nil)
	h.clk.Add(time.Minute)
	h.model.fn = func(ModelInput) string { return drink("water", 500, 0, 0, 0, false) }
	w := decode[LogResponse](t, h.logText("p0000002-0002", "water")).Items[0]
	h.clk.Add(time.Minute)
	var seen []LastItem
	h.model.fn = func(in ModelInput) string { seen = in.LastItems; return undoNamesOut([]string{"water"}, `"last"`) }
	r := decode[LogResponse](t, h.logText("p0000002-0003", "remove the water I just logged"))
	if len(r.Items) != 1 || r.Items[0].ItemID != w.ItemID || !strings.HasPrefix(r.Blocks[0].Text, "Removed water, 500 ml") {
		t.Fatalf("items %+v text %q", r.Items, r.Blocks[0].Text)
	}
	var sawCoffee bool
	for _, li := range seen {
		sawCoffee = sawCoffee || (li.Item == "filter coffee" && strings.HasPrefix(li.ItemID, "v:"))
	}
	if !sawCoffee {
		t.Fatalf("Telegram rows not shown to the model: %+v", seen)
	}
	// Even with which null and only ONE water left: removed, no question.
	h.model.fn = func(ModelInput) string { return drink("water", 250, 0, 0, 0, false) }
	w2 := decode[LogResponse](t, h.logText("p0000002-0004", "water")).Items[0]
	h.clk.Add(time.Minute)
	h.model.fn = func(ModelInput) string { return undoNamesOut([]string{"water"}, "null") }
	r2 := decode[LogResponse](t, h.logText("p0000002-0005", "remove the water"))
	if len(r2.Items) != 1 || r2.Items[0].ItemID != w2.ItemID {
		t.Fatalf("one water left: %+v %q", r2.Items, r2.Blocks[0].Text)
	}
}

func TestVagueRemovalAsksOnlyAmongTheNamedFood(t *testing.T) {
	h := newHarness(t)
	twoWaters(t, h)
	h.model.fn = func(ModelInput) string { return drink("sparkling water", 330, 0, 0, 0, false) }
	h.logText("p0000003-0001", "sparkling water")
	h.model.fn = func(ModelInput) string { return oneItem("skyr", 250, 157.5, 27.5, false) }
	h.logText("p0000003-0002", "skyr")
	n := len(h.vars.rows("var-food"))
	h.model.fn = func(ModelInput) string { return undoNamesOut([]string{"water"}, "null") }
	r := decode[LogResponse](t, h.logText("p0000003-0003", "remove the water"))
	txt := r.Blocks[0].Text
	if len(h.vars.rows("var-food")) != n || !strings.HasPrefix(txt, "Which one do you mean: water, 250 ml (09:12) or water, 500 ml (09:15)?") {
		t.Fatalf("text %q", txt)
	}
	if strings.Contains(txt, "skyr") || strings.Contains(txt, "sparkling") {
		t.Fatalf("listed other foods: %q", txt)
	}
	// A synonym counts only when the model maps it.
	h.model.fn = func(ModelInput) string { return undoNamesOut([]string{"sparkling water"}, "null") }
	r2 := decode[LogResponse](t, h.logText("p0000003-0004", "remove the sparkling one"))
	if len(r2.Items) != 1 || r2.Items[0].Item != "sparkling water" {
		t.Fatalf("%+v %q", r2.Items, r2.Blocks[0].Text)
	}
	// Unknown name: no write.
	h.model.fn = func(ModelInput) string { return undoNamesOut([]string{"tea"}, "null") }
	n = len(h.vars.rows("var-food"))
	r3 := decode[LogResponse](t, h.logText("p0000003-0005", "remove the tea"))
	if len(h.vars.rows("var-food")) != n || !strings.Contains(r3.Blocks[0].Text, "could not find tea") {
		t.Fatalf("%q", r3.Blocks[0].Text)
	}
}

func TestPhotosAllOrNone(t *testing.T) {
	h := newHarness(t)
	n := 0
	h.svc.photos.failSave = func() bool { n++; return n == 2 }
	body, ct := multipartBody(t, map[string]string{"client_id": "p0000004-0001"}, []filePart{
		{"image", "a.jpg", "image/jpeg", testJPEG(100, 100)},
		{"image", "b.jpg", "image/jpeg", testJPEG(100, 100)},
	})
	if r := h.do("POST", "/fuel/log", body, ct); r.Code != 500 {
		t.Fatalf("%d", r.Code)
	}
	if len(h.vars.rows("var-food")) != 0 || len(h.svc.journal.Entries()) != 0 || len(h.svc.photos.meta) != 0 {
		t.Fatalf("partial write: rows %d entries %d photos %d", len(h.vars.rows("var-food")), len(h.svc.journal.Entries()), len(h.svc.photos.meta))
	}
	h.svc.photos.failSave = nil
	body, ct = multipartBody(t, map[string]string{"client_id": "p0000004-0001"}, []filePart{
		{"image", "a.jpg", "image/jpeg", testJPEG(100, 100)},
		{"image", "b.jpg", "image/jpeg", testJPEG(100, 100)},
	})
	if r := decode[LogResponse](t, h.do("POST", "/fuel/log", body, ct)); len(r.PhotoIDs) != 2 {
		t.Fatalf("retry photo_ids %v", r.PhotoIDs)
	}
}

func TestNamesConstrainAnIDRef(t *testing.T) {
	h := newHarness(t)
	cof := agentdRow(h, "filter coffee", 300, 2, "2026-10-01T11:58:00+02:00", nil)
	h.model.fn = func(ModelInput) string { return oneItem("skyr", 250, 157.5, 27.5, false) }
	skyr := decode[LogResponse](t, h.logText("p0000005-0001", "skyr")).Items[0]
	h.clk.Add(time.Minute)
	n := len(h.vars.rows("var-food"))
	for i, ref := range []string{"v:" + cof, skyr.ItemID} {
		h.model.fn = func(ModelInput) string {
			return fmt.Sprintf(`{"intent":"undo","items":[],"corrections":[],"targets":[{"ref":%q,"names":["water"],"which":null,"volume_ml":null,"portion_g":null}],"text":"","widgets":[]}`, ref)
		}
		r := decode[LogResponse](t, h.logText(fmt.Sprintf("p0000005-%04d", i+2), "remove the water"))
		if len(r.Items) != 0 || len(h.vars.rows("var-food")) != n {
			t.Fatalf("ref %s removed %+v", ref, r.Items)
		}
	}
}

func TestPhotoIndexFailureLeavesNoFile(t *testing.T) {
	h := newHarness(t)
	h.svc.photos.failIdx = func() bool { return true }
	body, ct := multipartBody(t, map[string]string{"client_id": "p0000006-0001"}, []filePart{{"image", "a.jpg", "image/jpeg", testJPEG(100, 100)}})
	if r := h.do("POST", "/fuel/log", body, ct); r.Code != 500 {
		t.Fatalf("%d", r.Code)
	}
	entries, _ := os.ReadDir(filepath.Join(h.opts.StateDir, "photos"))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".jpg") {
			t.Fatalf("orphan photo file %s", e.Name())
		}
	}
}

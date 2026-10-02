// Command fuel-op is the client an agent uses to read and write Fuel data
// through fueld's deterministic routes (docs/specs/2026-10-fuel-api.md,
// sections 20 and 22.5). It prints plain text for the agent. A write needs
// the capability of the open chat turn (--turn); a read needs none.
//
// Config: the file fuel-op.conf in the working directory (or the path in
// FUEL_OP_CONF) with two lines, no secret:
//
//	url = http://100.120.65.8:8796
//	token_entry = fuel/agent-token
//
// The agent token is read with `pass show <token_entry>` inside the process
// for each call. It is never an argument, never in a file and never printed.
package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
)

const help = `fuel-op: read and write Fuel data through fueld. Plain text out.

Reads (no capability):
  fuel-op day [--date YYYY-MM-DD]        the active items of a day, with ids, and the budgets
  fuel-op snapshot [--date D]            the budgets, levers and intake of a day
  fuel-op week [--date D]                the week budgets
  fuel-op recent [--limit N]             the usual foods with their relog keys
  fuel-op feed [--limit N]               the last chat lines
  fuel-op preview [--day D] '<JSON list of items>'   what the items WOULD add; writes nothing
  fuel-op schema                         the item fields
  fuel-op help

Writes (need --turn <capability of the current FUEL TURN>):
  fuel-op items --turn C [--day today|yesterday|YYYY-MM-DD] [--time HH:MM] [--new] '<JSON list of items>'
  fuel-op fix   --turn C <item id> (--portion G | --volume ML | --share X | --add-g G | --add-ml ML | --add-count N | --revised '<JSON object>')
  fuel-op undo  --turn C <item id>
  fuel-op move  --turn C --day today|yesterday|YYYY-MM-DD <item id> [<item id> ...]
  fuel-op relog --turn C [--scale 0.5|1|1.5|2] [--new] <recent key>

Results of a write: "written", "pending" (it is being saved: do NOT write it again),
"refused" (nothing was written; read the reason), "unknown" (fueld did not answer:
write nothing more in this turn and say so).
`

const schema = `An item (for items, preview and --revised):
  required: item (name), kcal, protein_g, carbs_g (TOTAL, with fibre), fat_g, sat_fat_g (always a number)
  optional: portion_g, portion_basis (stated | label | scale | photo_estimate | unspecified),
            fiber_g (null = not known), net_carbs_g, kind (food | drink | supplement),
            volume_ml (drinks), caffeine_mg, alcohol_g, staple_key, needs_fraction,
            food_class (leafy_vegetable | vegetable | fruit | meat_fish | dairy | grain_starch | nuts_seeds | oil_fat | sweet | mixed_dish | drink | supplement | other),
            levers: {psyllium_g, beta_glucan_g, nuts_g, pulses_g, plant_protein_g, brew_method}
              (all six keys; a number, or null = not known, 0 = none;
               brew_method: filtered | unfiltered | espresso | instant | unknown, null for no coffee)
  --revised takes: item, portion_g, kcal, protein_g, carbs_g, net_carbs_g, fat_g, sat_fat_g, fiber_g, food_class, levers
Bounds: macros 0 to 300 g, kcal 0 to 3000, portion_g and volume_ml up to 5000. At most 12 items in a call.
`

type config struct{ url, tokenEntry string }

func loadConfig() (config, error) {
	path := os.Getenv("FUEL_OP_CONF")
	if path == "" {
		path = "fuel-op.conf"
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return config{}, fmt.Errorf("no config file %s (fuel-op runs in the Fuel workspace)", path)
	}
	var c config
	for _, line := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		v = strings.Trim(strings.TrimSpace(v), `"`)
		switch strings.TrimSpace(k) {
		case "url":
			c.url = strings.TrimRight(v, "/")
		case "token_entry":
			c.tokenEntry = v
		}
	}
	if c.url == "" || c.tokenEntry == "" {
		return c, errors.New("the config file needs url and token_entry")
	}
	return c, nil
}

// token reads the agent token from pass. Tests replace it.
var token = func(entry string) (string, error) {
	out, err := exec.Command("pass", "show", entry).Output()
	if err != nil {
		return "", errors.New("could not read the agent token from pass")
	}
	t := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
	if t == "" {
		return "", errors.New("the agent token is empty")
	}
	return t, nil
}

// args are the parsed arguments of a subcommand: --name value, --name=value,
// the boolean --new, and positional words, in any order.
type args struct {
	flags map[string]string
	pos   []string
}

func parseArgs(in []string) (args, error) {
	a := args{flags: map[string]string{}}
	for i := 0; i < len(in); i++ {
		w := in[i]
		if !strings.HasPrefix(w, "--") || len(w) < 3 {
			a.pos = append(a.pos, w)
			continue
		}
		name, val, has := strings.Cut(w[2:], "=")
		if name == "new" {
			a.flags["new"] = "true"
			continue
		}
		if !has {
			if i+1 >= len(in) {
				return a, fmt.Errorf("--%s needs a value", name)
			}
			i++
			val = in[i]
		}
		a.flags[name] = val
	}
	return a, nil
}

func newClientID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "fo" + hex.EncodeToString(b)
}

type client struct {
	cfg  config
	tok  string
	http *http.Client
	out  io.Writer
	wait time.Duration // between retries
}

// call sends one request; a transport error is retried with the SAME body.
func (c *client) call(method, path, turn string, body any) (int, []byte, error) {
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	var last error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(c.wait)
		}
		req, err := http.NewRequest(method, c.cfg.url+path, bytes.NewReader(raw))
		if err != nil {
			return 0, nil, err
		}
		req.Header.Set("Authorization", "Bearer "+c.tok)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if turn != "" {
			req.Header.Set("X-Fuel-Turn", turn)
		}
		resp, err := c.http.Do(req)
		if err != nil {
			last = err
			continue
		}
		b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()
		if err != nil {
			last = err
			continue
		}
		if resp.StatusCode == http.StatusServiceUnavailable || resp.StatusCode == http.StatusGatewayTimeout {
			last = fmt.Errorf("HTTP %d", resp.StatusCode)
			continue // busy or a timeout before the write: the same request again
		}
		return resp.StatusCode, b, nil
	}
	return 0, nil, last
}

func num(v any) string {
	switch x := v.(type) {
	case nil:
		return "?"
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case string:
		return x
	}
	return fmt.Sprint(v)
}

func str(v any) string { s, _ := v.(string); return s }

func errText(b []byte) (code, msg string) {
	var e struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(b, &e)
	return e.Error.Code, e.Error.Message
}

// budgets prints the budget lines of a snapshot.
func (c *client) budgets(snap map[string]any) {
	list, _ := snap["budgets"].([]any)
	if len(list) == 0 {
		list, _ = snap["macros"].([]any)
	}
	for _, x := range list {
		m, _ := x.(map[string]any)
		line := fmt.Sprintf("  %s: %s", str(m["key"]), num(m["consumed"]))
		if t, ok := m["target"].(float64); ok {
			cons, _ := m["consumed"].(float64)
			line += fmt.Sprintf(" of %s %s, left %s", num(t), str(m["unit"]), num(round1(t-cons)))
		} else {
			line += " " + str(m["unit"]) + " (no target)"
		}
		if k, s := str(m["kind"]), str(m["status"]); k != "" {
			line += " (" + k + ", " + s + ")"
		}
		fmt.Fprintln(c.out, line)
	}
}

func round1(f float64) float64 { return float64(int64(f*10+sign(f)*0.5)) / 10 }
func sign(f float64) float64 {
	if f < 0 {
		return -1
	}
	return 1
}

func macroLine(m map[string]any) string {
	return fmt.Sprintf("%s kcal, protein %s g, carbs %s g, fat %s g, sat fat %s g, fibre %s g",
		num(m["kcal"]), num(m["protein_g"]), num(m["carbs_g"]), num(m["fat_g"]), num(m["sat_fat_g"]), num(m["fiber_g"]))
}

func (c *client) read(path string, q url.Values) (map[string]any, bool) {
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	code, b, err := c.call(http.MethodGet, path, "", nil)
	if err != nil {
		fmt.Fprintln(c.out, "error: fueld did not answer. Try once more, then say that the data is not reachable.")
		return nil, false
	}
	if code != http.StatusOK {
		ec, msg := errText(b)
		fmt.Fprintf(c.out, "error (%d %s): %s\n", code, ec, msg)
		return nil, false
	}
	var v map[string]any
	if json.Unmarshal(b, &v) != nil {
		fmt.Fprintln(c.out, "error: unreadable answer")
		return nil, false
	}
	return v, true
}

func (c *client) day(a args) int {
	q := url.Values{}
	if d := a.flags["date"]; d != "" {
		q.Set("date", d)
	}
	v, ok := c.read("/fuel/day", q)
	if !ok {
		return 1
	}
	items, _ := v["items"].([]any)
	fmt.Fprintf(c.out, "day %s: %d active items (newest first)\n", str(v["date"]), len(items))
	for _, x := range items {
		m, _ := x.(map[string]any)
		amt := "amount unknown"
		if p, ok := m["portion_g"].(float64); ok {
			amt = num(p) + " g"
		}
		if vol, ok := m["volume_ml"].(float64); ok && (str(m["kind"]) == "drink" || amt == "amount unknown") {
			amt = num(vol) + " ml"
		}
		mac, _ := m["macros"].(map[string]any)
		at := str(m["eaten_at"])
		if t, err := time.Parse(time.RFC3339, at); err == nil {
			at = t.Local().Format("15:04")
		}
		extra := ""
		if s := str(m["check"]); s != "" {
			extra = " | CHECK: " + s
		}
		fmt.Fprintf(c.out, "  %s | %s | %s (%s) | %s | %s | source %s%s\n", str(m["row_key"]), at, str(m["item"]), str(m["kind"]), amt, macroLine(mac), str(m["source"]), extra)
	}
	if snap, ok := v["snapshot"].(map[string]any); ok {
		fmt.Fprintln(c.out, "budgets:")
		c.budgets(snap)
	}
	return 0
}

func (c *client) snapshot(a args) int {
	q := url.Values{}
	if d := a.flags["date"]; d != "" {
		q.Set("date", d)
	}
	v, ok := c.read("/fuel/snapshot", q)
	if !ok {
		return 1
	}
	fmt.Fprintf(c.out, "snapshot of %s (%s, %s)\nbudgets:\n", str(v["date"]), str(v["day_type"]), str(v["day_class"]))
	c.budgets(v)
	for _, key := range []string{"levers", "intake"} {
		list, _ := v[key].([]any)
		if len(list) == 0 {
			continue
		}
		fmt.Fprintln(c.out, key+":")
		for _, x := range list {
			m, _ := x.(map[string]any)
			line := fmt.Sprintf("  %s: %s %s", str(m["key"]), num(m["consumed"]), str(m["unit"]))
			if r, ok := m["reference"].(float64); ok {
				line += ", reference " + num(r)
			}
			if t, ok := m["target"].(float64); ok {
				line += ", target " + num(t)
			}
			fmt.Fprintln(c.out, line)
		}
	}
	return 0
}

func (c *client) week(a args) int {
	q := url.Values{}
	if d := a.flags["date"]; d != "" {
		q.Set("date", d)
	}
	v, ok := c.read("/fuel/week", q)
	if !ok {
		return 1
	}
	// The week view is nested; its compact JSON is the most exact form.
	for _, k := range []string{"week", "budgets", "performance", "context"} {
		if x, ok := v[k]; ok {
			b, _ := json.Marshal(x)
			fmt.Fprintf(c.out, "%s: %s\n", k, b)
		}
	}
	return 0
}

func (c *client) recent(a args) int {
	q := url.Values{}
	if l := a.flags["limit"]; l != "" {
		q.Set("limit", l)
	}
	v, ok := c.read("/fuel/recent", q)
	if !ok {
		return 1
	}
	items, _ := v["items"].([]any)
	fmt.Fprintf(c.out, "recent: %d items (key | item | amount | values | times, last)\n", len(items))
	for _, x := range items {
		m, _ := x.(map[string]any)
		amt := ""
		if p, ok := m["portion_g"].(float64); ok {
			amt = num(p) + " g"
		} else if vol, ok := m["volume_ml"].(float64); ok {
			amt = num(vol) + " ml"
		}
		mac, _ := m["macros"].(map[string]any)
		last := str(m["last_eaten_at"])
		if len(last) >= 10 {
			last = last[:10]
		}
		fmt.Fprintf(c.out, "  %s | %s (%s) | %s | %s | %s times, last %s\n", str(m["key"]), str(m["item"]), str(m["kind"]), amt, macroLine(mac), num(m["times"]), last)
	}
	return 0
}

func (c *client) feed(a args) int {
	q := url.Values{}
	q.Set("limit", "20")
	if l := a.flags["limit"]; l != "" {
		q.Set("limit", l)
	}
	v, ok := c.read("/fuel/feed", q)
	if !ok {
		return 1
	}
	items, _ := v["items"].([]any)
	for _, x := range items {
		m, _ := x.(map[string]any)
		text := str(m["text"])
		if text == "" {
			var parts []string
			blocks, _ := m["blocks"].([]any)
			for _, b := range blocks {
				bm, _ := b.(map[string]any)
				if str(bm["type"]) == "text" {
					parts = append(parts, str(bm["text"]))
				}
			}
			text = strings.Join(parts, " ")
		}
		at := str(m["at"])
		if t, err := time.Parse(time.RFC3339, at); err == nil {
			at = t.Local().Format("2006-01-02 15:04")
		}
		fmt.Fprintf(c.out, "[%s] %s: %s\n", at, str(m["role"]), strings.Join(strings.Fields(text), " "))
	}
	return 0
}

func jsonList(s string) ([]any, error) {
	var v any
	dec := json.NewDecoder(strings.NewReader(s))
	if err := dec.Decode(&v); err != nil {
		return nil, errors.New("the items are not valid JSON")
	}
	switch x := v.(type) {
	case []any:
		return x, nil
	case map[string]any:
		return []any{x}, nil
	}
	return nil, errors.New("give a JSON list of items")
}

func (c *client) preview(a args) int {
	if len(a.pos) != 1 {
		fmt.Fprintln(c.out, "usage: fuel-op preview [--day D] '<JSON list of items>'")
		return 1
	}
	items, err := jsonList(a.pos[0])
	if err != nil {
		fmt.Fprintln(c.out, "error:", err)
		return 1
	}
	body := map[string]any{"items": items}
	if d := a.flags["day"]; d != "" {
		body["day"] = d
	}
	code, b, err := c.call(http.MethodPost, "/fuel/preview", "", body)
	if err != nil {
		fmt.Fprintln(c.out, "error: fueld did not answer.")
		return 1
	}
	if code != http.StatusOK {
		ec, msg := errText(b)
		fmt.Fprintf(c.out, "error (%d %s): %s\n", code, ec, msg)
		return 1
	}
	var v map[string]any
	_ = json.Unmarshal(b, &v)
	fmt.Fprintf(c.out, "preview for %s. NOTHING was written.\n", str(v["date"]))
	list, _ := v["items"].([]any)
	for _, x := range list {
		m, _ := x.(map[string]any)
		mac, _ := m["macros"].(map[string]any)
		amt := ""
		if p, ok := m["portion_g"].(float64); ok {
			amt = " " + num(p) + " g"
		}
		fmt.Fprintf(c.out, "  %s%s: %s\n", str(m["item"]), amt, macroLine(mac))
	}
	if sum, ok := v["sum"].(map[string]any); ok {
		fmt.Fprintf(c.out, "sum: %s\n", macroLine(sum))
	}
	fmt.Fprintln(c.out, "budgets (now, what it adds, after it, left after it):")
	bl, _ := v["budgets"].([]any)
	for _, x := range bl {
		m, _ := x.(map[string]any)
		line := fmt.Sprintf("  %s: now %s", str(m["key"]), num(m["consumed"]))
		if t, ok := m["target"].(float64); ok {
			line += " of " + num(t)
		}
		line += " " + str(m["unit"])
		if m["adds"] != nil {
			line += fmt.Sprintf(", adds %s, after %s", num(m["adds"]), num(m["after"]))
			if m["left_after"] != nil {
				line += ", left after " + num(m["left_after"])
			}
		} else {
			line += ", adds unknown"
		}
		fmt.Fprintln(c.out, line+" ("+str(m["kind"])+")")
	}
	return 0
}

// write sends one turn-bound write and prints one of the four results.
func (c *client) write(path, turn string, body map[string]any) int {
	if turn == "" {
		fmt.Fprintln(c.out, "error: a write needs --turn <capability of the current FUEL TURN>. Nothing was written.")
		return 1
	}
	body["client_id"] = newClientID()
	code, b, err := c.call(http.MethodPost, path, turn, body)
	if err != nil {
		fmt.Fprintln(c.out, "unknown: fueld did not answer. Write nothing more in this turn and say that you do not know if it was saved.")
		return 1
	}
	if code != http.StatusOK {
		ec, msg := errText(b)
		fmt.Fprintf(c.out, "refused (%d %s): %s\n", code, ec, msg)
		return 1
	}
	var v struct {
		Result string           `json:"result"`
		Items  []map[string]any `json:"items"`
		Lines  []string         `json:"lines"`
		Date   string           `json:"date"`
		Snap   map[string]any   `json:"snapshot"`
	}
	if json.Unmarshal(b, &v) != nil {
		fmt.Fprintln(c.out, "unknown: unreadable answer. Write nothing more in this turn.")
		return 1
	}
	switch v.Result {
	case "written":
		fmt.Fprintln(c.out, "written")
	case "pending":
		fmt.Fprintln(c.out, "pending: it is being saved. Do NOT write it again; say that it is being saved.")
	case "failed":
		fmt.Fprintln(c.out, "failed: the food log refused a row. Say what could not be saved.")
	default:
		fmt.Fprintln(c.out, "nothing to write: the item already has that amount")
	}
	for _, l := range v.Lines {
		fmt.Fprintln(c.out, "  "+l)
	}
	for _, m := range v.Items {
		eff, _ := m["effective"].(map[string]any)
		state := ""
		if u, _ := m["undone"].(bool); u {
			state = " (removed)"
		}
		chk := ""
		if s := str(m["check"]); s != "" {
			chk = " | CHECK FLAG: " + s
		}
		fmt.Fprintf(c.out, "  %s %s%s: now %s%s\n", str(m["item_id"]), str(m["item"]), state, macroLine(eff), chk)
	}
	fmt.Fprintf(c.out, "budgets of %s after this:\n", v.Date)
	c.budgets(v.Snap)
	return 0
}

func (c *client) items(a args) int {
	if len(a.pos) != 1 {
		fmt.Fprintln(c.out, "usage: fuel-op items --turn C [--day D] [--time HH:MM] [--new] '<JSON list of items>'. Nothing was written.")
		return 1
	}
	items, err := jsonList(a.pos[0])
	if err != nil {
		fmt.Fprintln(c.out, "error:", err, "Nothing was written.")
		return 1
	}
	body := map[string]any{"items": items}
	if d := a.flags["day"]; d != "" {
		body["day"] = d
	}
	if t := a.flags["time"]; t != "" {
		body["time"] = t
	}
	if a.flags["new"] != "" {
		body["new"] = true
	}
	return c.write("/fuel/items", a.flags["turn"], body)
}

func (c *client) fix(a args) int {
	if len(a.pos) != 1 {
		fmt.Fprintln(c.out, "usage: fuel-op fix --turn C <item id> (--portion G | --volume ML | --share X | --add-g G | --add-ml ML | --add-count N | --revised '<JSON>'). Nothing was written.")
		return 1
	}
	body := map[string]any{"item_id": a.pos[0]}
	forms := map[string]string{"portion": "portion_g", "volume": "volume_ml", "share": "share", "add-g": "portion_g_delta", "add-ml": "volume_ml_delta", "add-count": "count_delta"}
	n := 0
	for flag, key := range forms {
		if v, ok := a.flags[flag]; ok {
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				fmt.Fprintf(c.out, "error: --%s needs a number. Nothing was written.\n", flag)
				return 1
			}
			body[key] = f
			n++
		}
	}
	if r, ok := a.flags["revised"]; ok {
		var v map[string]any
		if json.Unmarshal([]byte(r), &v) != nil {
			fmt.Fprintln(c.out, "error: --revised needs a JSON object. Nothing was written.")
			return 1
		}
		body["revised"] = v
		n++
	}
	if n != 1 {
		fmt.Fprintln(c.out, "error: give exactly one of --portion, --volume, --share, --add-g, --add-ml, --add-count, --revised. Nothing was written.")
		return 1
	}
	return c.write("/fuel/fix", a.flags["turn"], body)
}

func (c *client) undo(a args) int {
	if len(a.pos) != 1 {
		fmt.Fprintln(c.out, "usage: fuel-op undo --turn C <item id>. Nothing was written.")
		return 1
	}
	return c.write("/fuel/undo", a.flags["turn"], map[string]any{"item_id": a.pos[0]})
}

func (c *client) move(a args) int {
	if len(a.pos) == 0 || a.flags["day"] == "" {
		fmt.Fprintln(c.out, "usage: fuel-op move --turn C --day today|yesterday|YYYY-MM-DD <item id> [<item id> ...]. Nothing was written.")
		return 1
	}
	ids := make([]any, 0, len(a.pos))
	for _, p := range a.pos {
		ids = append(ids, p)
	}
	return c.write("/fuel/move", a.flags["turn"], map[string]any{"day": a.flags["day"], "items": ids})
}

func (c *client) relog(a args) int {
	if len(a.pos) != 1 {
		fmt.Fprintln(c.out, "usage: fuel-op relog --turn C [--scale 0.5|1|1.5|2] [--new] <recent key>. Nothing was written.")
		return 1
	}
	body := map[string]any{"key": a.pos[0]}
	if s, ok := a.flags["scale"]; ok {
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			fmt.Fprintln(c.out, "error: --scale needs a number. Nothing was written.")
			return 1
		}
		body["scale"] = f
	}
	if a.flags["new"] != "" {
		body["new"] = true
	}
	return c.write("/fuel/relog", a.flags["turn"], body)
}

func run(argv []string, out io.Writer) int {
	if len(argv) == 0 || argv[0] == "help" || argv[0] == "--help" || argv[0] == "-h" {
		fmt.Fprint(out, help)
		return 0
	}
	if argv[0] == "schema" {
		fmt.Fprint(out, schema)
		return 0
	}
	cmds := []string{"day", "snapshot", "week", "recent", "feed", "preview", "items", "fix", "undo", "move", "relog"}
	sort.Strings(cmds)
	if i := sort.SearchStrings(cmds, argv[0]); i >= len(cmds) || cmds[i] != argv[0] {
		fmt.Fprintf(out, "error: unknown command %q. Run fuel-op help.\n", argv[0])
		return 1
	}
	a, err := parseArgs(argv[1:])
	if err != nil {
		fmt.Fprintln(out, "error:", err)
		return 1
	}
	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintln(out, "error:", err)
		return 1
	}
	tok, err := token(cfg.tokenEntry)
	if err != nil {
		fmt.Fprintln(out, "error:", err)
		return 1
	}
	c := &client{cfg: cfg, tok: tok, http: &http.Client{Timeout: 45 * time.Second}, out: out, wait: time.Second}
	switch argv[0] {
	case "day":
		return c.day(a)
	case "snapshot":
		return c.snapshot(a)
	case "week":
		return c.week(a)
	case "recent":
		return c.recent(a)
	case "feed":
		return c.feed(a)
	case "preview":
		return c.preview(a)
	case "items":
		return c.items(a)
	case "fix":
		return c.fix(a)
	case "undo":
		return c.undo(a)
	case "move":
		return c.move(a)
	}
	return c.relog(a)
}

func main() { os.Exit(run(os.Args[1:], os.Stdout)) }

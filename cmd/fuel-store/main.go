// fuel-store migrates and verifies the Fuel hybrid store. It never posts to Variables.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/kjhnns/agentd/internal/fuel"
	"github.com/kjhnns/agentd/internal/fuel/sqlstore"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, out, errOut io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(errOut, "usage: fuel-store {import|compare|backup|restore-check} [flags]")
		return 2
	}
	var err error
	switch args[0] {
	case "import":
		err = importCommand(ctx, args[1:], out, errOut)
	case "compare":
		err = compareCommand(ctx, args[1:], out, errOut)
	case "backup":
		err = backupCommand(ctx, args[1:], out, errOut, time.Now())
	case "restore-check":
		err = restoreCommand(ctx, args[1:], out, errOut)
	default:
		err = fmt.Errorf("unknown command %q", args[0])
	}
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		fmt.Fprintln(errOut, err)
		if errors.Is(err, errDifferent) {
			return 1
		}
		return 2
	}
	return 0
}

var errDifferent = errors.New("daily totals differ")

var variablesClient = &http.Client{Timeout: 2 * time.Minute}

type remoteFlags struct{ url, key, food, body, bp, symptom string }

func (r *remoteFlags) flags(f *flag.FlagSet, records bool) {
	f.StringVar(&r.url, "variables-url", "", "Variables ext API base URL")
	f.StringVar(&r.key, "key-env", "VARIABLES_API_KEY", "environment variable holding the API key")
	f.StringVar(&r.food, "food-var", "", "Food log variable ID")
	if records {
		f.StringVar(&r.body, "body-var", "", "Body composition variable ID")
		f.StringVar(&r.bp, "bp-var", "", "Blood pressure variable ID (default: discover by name)")
		f.StringVar(&r.symptom, "symptom-var", "", "Symptom log variable ID (default: discover by name)")
	}
}
func (r remoteFlags) read(ctx context.Context, records bool) ([]fuel.Value, fuel.VarIDs, error) {
	ids := fuel.VarIDs{Food: r.food, Body: r.body, BP: r.bp, Symptom: r.symptom}
	if r.url == "" || r.food == "" || (records && r.body == "") {
		return nil, ids, errors.New("variables-url, food-var and (for import) body-var are required")
	}
	key := os.Getenv(r.key)
	if key == "" {
		return nil, ids, fmt.Errorf("key environment variable %q is empty", r.key)
	}
	v := &fuel.VariablesHTTP{Base: r.url, Key: key, Client: variablesClient}
	vars, err := v.ListVariables(ctx)
	if err != nil {
		return nil, ids, err
	}
	if records {
		for _, target := range []struct {
			name string
			id   *string
		}{{"Blood pressure", &ids.BP}, {"Symptom log", &ids.Symptom}} {
			if *target.id != "" {
				continue
			}
			for _, vr := range vars {
				if vr.Name == target.name {
					if *target.id != "" {
						return nil, ids, fmt.Errorf("variable name %q is ambiguous; specify its ID", target.name)
					}
					*target.id = vr.ID
				}
			}
		}
	}
	seen := map[string]bool{}
	for _, id := range []string{ids.Food, ids.Body, ids.BP, ids.Symptom} {
		if id == "" {
			continue
		}
		if seen[id] {
			return nil, ids, errors.New("variable IDs must be distinct")
		}
		seen[id] = true
		found := false
		for _, vr := range vars {
			if vr.ID == id {
				found = true
				if vr.Type != "json" {
					return nil, ids, fmt.Errorf("variable %q is not JSON", id)
				}
			}
		}
		if !found {
			return nil, ids, fmt.Errorf("variable %q was not found", id)
		}
	}
	values, err := v.All(ctx)
	return values, ids, err
}
func parse(f *flag.FlagSet, args []string) error {
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	return nil
}
func flags(name string, errOut io.Writer) *flag.FlagSet {
	f := flag.NewFlagSet(name, flag.ContinueOnError)
	f.SetOutput(errOut)
	return f
}

func importCommand(ctx context.Context, args []string, out, errOut io.Writer) error {
	f := flags("import", errOut)
	var remote remoteFlags
	remote.flags(f, true)
	db := f.String("db", "", "SQLite database path")
	state := f.String("state-dir", "", "legacy state directory")
	dry := f.Bool("dry-run", false, "validate and summarize using a temporary database")
	if err := parse(f, args); err != nil {
		return err
	}
	if *db == "" || *state == "" {
		return errors.New("db and state-dir are required")
	}
	batch, warnings, err := fuel.ReadLegacyState(*state)
	if err != nil {
		return err
	}
	for _, w := range warnings {
		fmt.Fprintln(errOut, "warning:", w)
	}
	values, ids, err := remote.read(ctx, true)
	if err != nil {
		return err
	}
	batch.Food, batch.Records, err = fuel.HistoryImportRows(values, ids)
	if err != nil {
		return err
	}
	path := *db
	if *dry {
		dir, err := os.MkdirTemp("", "fuel-store-dry-run-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(dir)
		path = filepath.Join(dir, "fuel.sqlite")
		if _, err := os.Stat(*db); err == nil {
			source, err := sqlstore.OpenReadOnly(*db)
			if err != nil {
				return err
			}
			err = source.Backup(ctx, path)
			err = errors.Join(err, source.Close())
			if err != nil {
				return err
			}
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	s, err := sqlstore.Open(path)
	if err != nil {
		return err
	}
	defer s.Close()
	summary, err := s.Import(ctx, batch)
	if err != nil {
		return err
	}
	label := "import"
	if *dry {
		label = "dry-run"
	}
	fmt.Fprintf(out, "%s: state lines +%d; food +%d updated %d; remote records +%d updated %d\n", label, summary.Lines, summary.FoodAdded, summary.FoodUpdated, summary.RecordsAdded, summary.RecordsUpdated)
	return nil
}

var macroKeys = []string{"kcal", "protein_g", "sat_fat_g", "fiber_g", "carbs_g"}

func compareCommand(ctx context.Context, args []string, out, errOut io.Writer) error {
	f := flags("compare", errOut)
	var remote remoteFlags
	remote.flags(f, false)
	db := f.String("db", "", "SQLite database path")
	from := f.String("from", "", "inclusive YYYY-MM-DD")
	through := f.String("to", "", "inclusive YYYY-MM-DD")
	if err := parse(f, args); err != nil {
		return err
	}
	start, e1 := time.Parse("2006-01-02", *from)
	end, e2 := time.Parse("2006-01-02", *through)
	if *db == "" || e1 != nil || e2 != nil || start.After(end) {
		return errors.New("db and a valid inclusive --from/--to date range are required")
	}
	values, _, err := remote.read(ctx, false)
	if err != nil {
		return err
	}
	// Validate remote food before reducing it. Otherwise malformed upstream rows
	// could appear as zero totals and hide a migration gap.
	if _, _, err := fuel.HistoryImportRows(values, fuel.VarIDs{Food: remote.food}); err != nil {
		return err
	}
	s, err := sqlstore.OpenReadOnly(*db)
	if err != nil {
		return err
	}
	defer s.Close()
	// Read all dates so moves from outside the comparison range retain the
	// same source visibility as Variables. Use the remote alias for corrections.
	rows, err := s.ListRows(ctx, "0001-01-01", "9999-12-31")
	if err != nil {
		return err
	}
	local := make([]fuel.Value, 0, len(rows))
	for _, r := range rows {
		var data map[string]any
		if err := json.Unmarshal(r.Data, &data); err != nil {
			return err
		}
		id := r.VariablesValueID
		if id == "" {
			id = r.ID
		}
		local = append(local, fuel.Value{ID: id, VariableID: remote.food, RecordDate: r.RecordDate, CreatedAt: r.CreatedAt, Raw: string(r.Data), Data: data})
	}
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "date\tsource\tkcal\tprotein\tsat fat\tfibre\tcarbs\titems\tmatch")
	different := false
	localTotals := fuel.HistoryTotals(local, remote.food)
	remoteTotals := fuel.HistoryTotals(values, remote.food)
	for date := start; !date.After(end); date = date.AddDate(0, 0, 1) {
		day := date.Format("2006-01-02")
		a, ai := localTotals[day].Totals, localTotals[day].Items
		b, bi := remoteTotals[day].Totals, remoteTotals[day].Items
		same := ai == bi
		for _, key := range macroKeys {
			same = same && a.Sum[key] == b.Sum[key] && a.Unknown[key] == b.Unknown[key]
		}
		result := "ok"
		if !same {
			result = "DIFF"
			different = true
		}
		for _, row := range []struct {
			name string
			t    fuel.DayTotals
			n    int
		}{{"store", a, ai}, {"variables", b, bi}} {
			fmt.Fprintf(w, "%s\t%s", day, row.name)
			for _, key := range macroKeys {
				value := fmt.Sprintf("%.1f", float64(row.t.Sum[key])/10)
				if row.t.Unknown[key] > 0 {
					value += fmt.Sprintf("+?%d", row.t.Unknown[key])
				}
				fmt.Fprintf(w, "\t%s", value)
			}
			fmt.Fprintf(w, "\t%d\t%s\n", row.n, result)
		}
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if different {
		return errDifferent
	}
	return nil
}

func backupCommand(ctx context.Context, args []string, out, errOut io.Writer, now time.Time) error {
	f := flags("backup", errOut)
	db := f.String("db", "", "existing SQLite database")
	dir := f.String("out", "", "backup directory")
	keep := f.Int("keep", 7, "number of snapshots to retain; 0 keeps all")
	if err := parse(f, args); err != nil {
		return err
	}
	if *db == "" || *dir == "" || *keep < 0 {
		return errors.New("db, out and nonnegative keep are required")
	}
	s, err := sqlstore.OpenReadOnly(*db)
	if err != nil {
		return err
	}
	defer s.Close()
	if err := os.MkdirAll(*dir, 0700); err != nil {
		return err
	}
	dest := filepath.Join(*dir, "fuel-"+now.UTC().Format("20060102-1504")+".db")
	if err := s.Backup(ctx, dest); err != nil {
		return err
	}
	if err := rotate(*dir, *db, *keep); err != nil {
		return fmt.Errorf("backup verified at %s; rotation failed: %w", dest, err)
	}
	fmt.Fprintf(out, "%s: integrity_check ok\n", dest)
	return nil
}
func rotate(dir, source string, keep int) error {
	if keep == 0 {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	sourceInfo, err := os.Stat(source)
	if err != nil {
		return err
	}
	var paths []string
	for _, e := range entries {
		name := e.Name()
		if e.Type()&os.ModeSymlink != 0 || e.IsDir() || !strings.HasPrefix(name, "fuel-") || !strings.HasSuffix(name, ".db") {
			continue
		}
		stamp := strings.TrimSuffix(strings.TrimPrefix(name, "fuel-"), ".db")
		if _, err := time.Parse("20060102-1504", stamp); err != nil {
			continue
		}
		info, err := e.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || os.SameFile(sourceInfo, info) {
			continue
		}
		paths = append(paths, filepath.Join(dir, name))
	}
	sort.Sort(sort.Reverse(sort.StringSlice(paths)))
	for i := keep; i < len(paths); i++ {
		if err := os.Remove(paths[i]); err != nil {
			return err
		}
	}
	return nil
}
func restoreCommand(ctx context.Context, args []string, out, errOut io.Writer) error {
	f := flags("restore-check", errOut)
	path := f.String("backup", "", "snapshot file")
	if err := parse(f, args); err != nil {
		return err
	}
	if *path == "" {
		return errors.New("backup is required")
	}
	s, err := sqlstore.OpenReadOnly(*path)
	if err != nil {
		return err
	}
	defer s.Close()
	if err := s.IntegrityCheck(ctx); err != nil {
		return err
	}
	counts, err := s.RowCounts(ctx)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(counts))
	for name := range counts {
		names = append(names, name)
	}
	sort.Strings(names)
	fmt.Fprintln(out, "integrity_check ok (read-only)")
	for _, name := range names {
		fmt.Fprintf(out, "%s\t%d\n", name, counts[name])
	}
	return nil
}

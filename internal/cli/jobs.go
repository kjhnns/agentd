package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/kjhnns/agentd/internal/scheduler"
)

// Jobs is the local ops surface for the scheduler: it talks to the RUNNING
// daemon's API (bind + bearer from config.toml), so lists reflect live state.
//
//	agentd jobs list          jobs + next fire + last run status
//	agentd jobs run <name>    fire a job now (manual trigger)
//	agentd jobs runs <name>   recent run history for a job
func Jobs(args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		fmt.Fprintln(stderr, "usage: agentd jobs <list|run NAME|runs NAME> [-config config.toml]")
		return ExitFailure
	}
	sub := args[0]
	fs := flag.NewFlagSet("jobs "+sub, flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "", "config.toml for bind + bearer (default: $AGENTD_CONFIG, ~/.agentd/config.toml, ./config.toml)")
	if err := fs.Parse(args[1:]); err != nil {
		if err == flag.ErrHelp {
			return ExitOK
		}
		return ExitUsage
	}
	addr, token, err := Endpoint(*cfgPath)
	if err != nil {
		fmt.Fprintf(stderr, "jobs: %v\n", err)
		return ExitFailure
	}
	c := NewClient(addr, token, 0)
	call := func(method, path string) ([]byte, error) {
		return c.do(context.Background(), method, path, nil, "")
	}

	switch sub {
	case "list":
		data, err := call(http.MethodGet, "/jobs")
		if err != nil {
			fmt.Fprintf(stderr, "jobs: %v\n", err)
			return ExitFailure
		}
		var jobs []scheduler.JobStatus
		if err := json.Unmarshal(data, &jobs); err != nil {
			fmt.Fprintf(stderr, "jobs list: %v\n", err)
			return ExitFailure
		}
		if len(jobs) == 0 {
			fmt.Fprintln(stdout, "no jobs configured (add [[job]] blocks to config.toml)")
			return ExitOK
		}
		w := tabwriter.NewWriter(stdout, 2, 4, 2, ' ', 0)
		fmt.Fprintln(w, "NAME\tENABLED\tTRIGGER\tWORKSPACE\tNOTIFY\tNEXT FIRE\tLAST RUN")
		for _, j := range jobs {
			next := "-"
			if j.NextFire != nil {
				next = j.NextFire.Local().Format("2006-01-02 15:04:05")
			}
			last := "-"
			if j.LastRun != nil {
				last = fmt.Sprintf("%s %s (%s)", j.LastRun.Status, j.LastRun.Start.Local().Format("01-02 15:04"), j.LastRun.Trigger)
			}
			ws := j.Workspace
			if ws == "" {
				ws = "(default)"
			}
			fmt.Fprintf(w, "%s\t%v\t%s\t%s\t%s\t%s\t%s\n", j.Name, j.Enabled, j.Trigger, ws, j.Notify, next, last)
		}
		_ = w.Flush()
	case "run":
		name := fs.Arg(0)
		if name == "" {
			fmt.Fprintln(stderr, "usage: agentd jobs run NAME")
			return ExitFailure
		}
		data, err := call(http.MethodPost, "/jobs/"+url.PathEscape(name)+"/run")
		if err != nil {
			fmt.Fprintf(stderr, "jobs: %v\n", err)
			return ExitFailure
		}
		fmt.Fprintln(stdout, strings.TrimSpace(string(data)))
	case "runs":
		name := fs.Arg(0)
		if name == "" {
			fmt.Fprintln(stderr, "usage: agentd jobs runs NAME")
			return ExitFailure
		}
		data, err := call(http.MethodGet, "/jobs/"+url.PathEscape(name)+"/runs")
		if err != nil {
			fmt.Fprintf(stderr, "jobs: %v\n", err)
			return ExitFailure
		}
		var runs []scheduler.JobRun
		if err := json.Unmarshal(data, &runs); err != nil {
			fmt.Fprintf(stderr, "jobs runs: %v\n", err)
			return ExitFailure
		}
		if len(runs) == 0 {
			fmt.Fprintln(stdout, "no runs recorded")
			return ExitOK
		}
		w := tabwriter.NewWriter(stdout, 2, 4, 2, ' ', 0)
		fmt.Fprintln(w, "RUN\tTRIGGER\tSTART\tDURATION\tSTATUS\tRESULT/ERROR")
		for _, r := range runs {
			dur := "-"
			if !r.End.IsZero() {
				dur = r.End.Sub(r.Start).Round(time.Millisecond).String()
			}
			msg := r.Result
			if r.Error != "" {
				msg = r.Error
			}
			if len(msg) > 80 {
				msg = msg[:80] + "..."
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", r.RunID, r.Trigger, r.Start.Local().Format("01-02 15:04:05"), dur, r.Status, msg)
		}
		_ = w.Flush()
	default:
		fmt.Fprintf(stderr, "jobs: unknown subcommand %q (want list|run|runs)\n", sub)
		return ExitFailure
	}
	return ExitOK
}

package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"
)

// Run implements `agentd run "<prompt>"`: ONE turn against the running daemon,
// answer on stdout, everything else on stderr, meaningful exit code. This is
// the surface a shell script talks to, so it is deliberately quiet: no banner,
// no spinner, no progress, just the answer.
//
//	agentd run "what is on my calendar tomorrow"
//	echo "summarize this" | agentd run
//	agentd run -json -wait 90s "..."     # {"ok":true,"session":"...","result":"..."}
//	agentd run -file shot.png "what is this"
func Run(args []string, stdout, stderr io.Writer, stdin io.Reader) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "", "config.toml for bind + bearer (default: $AGENTD_CONFIG, ~/.agentd/config.toml, ./config.toml)")
	sessionID := fs.String("session", "", "target an existing live session by id")
	newSession := fs.Bool("new", false, "start a fresh session instead of reusing the persistent CLI one")
	title := fs.String("title", "", "title of the persistent CLI session (default: cli:$USER)")
	workspace := fs.String("workspace", "", "workspace for a newly created session (default: the server's default)")
	asJSON := fs.Bool("json", false, "emit one JSON object instead of bare text")
	wait := fs.Duration("wait", 0, "give up after this long (0 = wait for the daemon's own turn budget)")
	file := fs.String("file", "", "attach an image/audio/document to the turn (core media ingest)")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: agentd run [flags] \"<prompt>\"   (prompt is read from stdin when omitted)")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}

	prompt := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if prompt == "" || prompt == "-" {
		data, err := io.ReadAll(stdin)
		if err != nil {
			fmt.Fprintf(stderr, "agentd run: reading prompt from stdin: %v\n", err)
			return ExitUsage
		}
		prompt = strings.TrimSpace(string(data))
	}
	if prompt == "" && *file == "" {
		fs.Usage()
		return ExitUsage
	}

	addr, token, err := Endpoint(*cfgPath)
	if err != nil {
		fmt.Fprintf(stderr, "agentd run: %v\n", err)
		return ExitUsage
	}
	// The HTTP client stays unbounded when -wait is unset: a real turn runs for
	// minutes and the DAEMON already owns the turn budget ([session]
	// turn_timeout), which it reports as a 504.
	c := NewClient(addr, token, *wait)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	started := time.Now()
	result, id, err := runTurn(ctx, c, Target{
		ID: *sessionID, New: *newSession, Title: *title, Workspace: *workspace,
	}, prompt, *file, stderr)

	if *asJSON {
		out := map[string]any{
			"ok":          err == nil,
			"session":     id,
			"result":      result,
			"duration_ms": time.Since(started).Milliseconds(),
		}
		if err != nil {
			out["error"] = err.Error()
			out["exit"] = exitCode(err)
		}
		enc := json.NewEncoder(stdout)
		enc.SetEscapeHTML(false)
		_ = enc.Encode(out)
		return exitCode(err)
	}
	if err != nil {
		fmt.Fprintf(stderr, "agentd run: %v\n", err)
		return exitCode(err)
	}
	fmt.Fprintln(stdout, result)
	return ExitOK
}

// runTurn resolves the session, wires Ctrl-C to the interrupt endpoint (so the
// SESSION survives an abandoned turn), and sends the turn. It returns the
// session id even on failure so -json can report which session was targeted.
func runTurn(ctx context.Context, c *Client, t Target, prompt, file string, stderr io.Writer) (string, string, error) {
	if err := c.Health(ctx); err != nil {
		return "", "", err
	}
	id, _, err := c.Resolve(ctx, t)
	if err != nil {
		return "", "", err
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sig)
	turnCtx, cancelTurn := context.WithCancel(ctx)
	defer cancelTurn()
	go func() {
		select {
		case <-sig:
			fmt.Fprintln(stderr, "\ninterrupting the turn (the session stays alive)")
			ictx, icancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer icancel()
			if ierr := c.Interrupt(ictx, id); ierr != nil {
				fmt.Fprintf(stderr, "agentd: interrupt failed: %v\n", ierr)
			}
			cancelTurn()
		case <-turnCtx.Done():
		}
	}()

	var result string
	if file != "" {
		result, err = c.SendFile(turnCtx, id, file, prompt)
	} else {
		result, err = c.Send(turnCtx, id, prompt)
	}
	if err != nil {
		return "", id, err
	}
	return result, id, nil
}

// Sessions implements `agentd sessions`: the live sessions the daemon holds,
// which is how a CLI user finds an id for -session (and sees that the CLI's own
// session is a first-class session next to the telegram/web ones).
func Sessions(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("sessions", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "", "config.toml for bind + bearer")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	addr, token, err := Endpoint(*cfgPath)
	if err != nil {
		fmt.Fprintf(stderr, "agentd sessions: %v\n", err)
		return ExitUsage
	}
	c := NewClient(addr, token, 30*time.Second)
	list, err := c.Sessions(context.Background())
	if err != nil {
		fmt.Fprintf(stderr, "agentd sessions: %v\n", err)
		return exitCode(err)
	}
	if len(list) == 0 {
		fmt.Fprintln(stdout, "no live sessions")
		return ExitOK
	}
	w := tabwriter.NewWriter(stdout, 2, 4, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tTITLE\tSTATUS\tAGENT_OK\tTURNS\tPRESSURE")
	for _, s := range list {
		fmt.Fprintf(w, "%s\t%s\t%s\t%v\t%d\t%.2f (%s)\n", s.ID, s.Title, s.Status, s.AgentOK, s.Turns, s.Pressure, s.PressureSource)
	}
	_ = w.Flush()
	return ExitOK
}

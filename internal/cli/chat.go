package cli

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/kjhnns/agentd/internal/eventbus"
)

// osExit is indirected so the REPL's "Ctrl-C at an idle prompt quits" path is
// testable and so nothing else in the package reaches for os.Exit.
var osExit = os.Exit

// Chat implements `agentd chat`: an interactive REPL against the RUNNING
// daemon. It attaches to the persistent CLI session (so the conversation is the
// same one `agentd run` uses and the web UI can read its history), streams the
// turn's events over the session WebSocket as they arrive, and shows a working
// indicator that is the terminal analogue of the Telegram emoji chain
// (working -> needs input -> done / error).
//
// Ctrl-C interrupts the CURRENT TURN via POST /sessions/:id/interrupt and keeps
// the session; Ctrl-C at an idle prompt (or EOF) quits.
func Chat(args []string, stdout, stderr io.Writer, stdin io.Reader) int {
	fs := flag.NewFlagSet("chat", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "", "config.toml for bind + bearer (default: $AGENTD_CONFIG, ~/.agentd/config.toml, ./config.toml)")
	sessionID := fs.String("session", "", "attach to an existing live session by id")
	newSession := fs.Bool("new", false, "start a fresh session instead of reusing the persistent CLI one")
	title := fs.String("title", "", "title of the persistent CLI session (default: cli:$USER)")
	workspace := fs.String("workspace", "", "workspace for a newly created session (default: the server's default)")
	noStream := fs.Bool("no-stream", false, "do not open the event WebSocket; print only the final answer")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: agentd chat [flags]    (interactive REPL against the running daemon)")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}

	addr, token, err := Endpoint(*cfgPath)
	if err != nil {
		fmt.Fprintf(stderr, "agentd chat: %v\n", err)
		return ExitUsage
	}
	c := NewClient(addr, token, 0) // a turn may legitimately run for minutes
	ctx := context.Background()
	if err := c.Health(ctx); err != nil {
		fmt.Fprintf(stderr, "agentd chat: %v\n", err)
		return exitCode(err)
	}
	id, created, err := c.Resolve(ctx, Target{
		ID: *sessionID, New: *newSession, Title: *title, Workspace: *workspace,
	})
	if err != nil {
		fmt.Fprintf(stderr, "agentd chat: %v\n", err)
		return exitCode(err)
	}

	p := &printer{out: stdout, err: stderr, tty: isTTY(stdout)}
	verb := "attached to"
	if created {
		verb = "started"
	}
	p.line(fmt.Sprintf("agentd %s session %s on %s", verb, id, addr))
	p.line("Ctrl-C interrupts the turn, Ctrl-D or /exit quits, /help lists commands")

	// Stream the session's events. A failed dial degrades to answer-only rather
	// than killing the REPL: the answer still comes back on the POST.
	var events <-chan eventbus.Event
	if !*noStream {
		ev, stop, derr := c.Events(id)
		if derr != nil {
			p.line("(event stream unavailable, showing final answers only: " + derr.Error() + ")")
		} else {
			defer stop()
			events = ev
		}
	}

	var turnActive atomic.Bool
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT)
	defer signal.Stop(sig)
	interrupt := make(chan struct{}, 1)
	go func() {
		for range sig {
			if !turnActive.Load() {
				p.line("")
				p.line("bye")
				osExit(ExitOK)
				return
			}
			select {
			case interrupt <- struct{}{}:
			default:
			}
		}
	}()

	sc := bufio.NewScanner(stdin)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024) // pasted prompts get long
	for {
		p.prompt("> ")
		if !sc.Scan() {
			p.line("bye")
			return ExitOK
		}
		text := strings.TrimSpace(sc.Text())
		if text == "" {
			continue
		}
		if strings.HasPrefix(text, "/") {
			done, newID := p.command(ctx, c, id, text)
			if newID != "" {
				id = newID
			}
			if done {
				return ExitOK
			}
			continue
		}

		turnActive.Store(true)
		err := p.turn(ctx, c, id, text, events, interrupt)
		turnActive.Store(false)
		if err != nil {
			p.state("error")
			p.line("error: " + err.Error())
			if exitCode(err) == ExitUnreachable {
				return ExitUnreachable
			}
		}
	}
}

// command handles the REPL's slash commands. It returns whether the REPL should
// quit, plus a replacement session id when the command switched sessions.
func (p *printer) command(ctx context.Context, c *Client, id, line string) (quit bool, newID string) {
	fields := strings.Fields(line)
	switch fields[0] {
	case "/exit", "/quit":
		p.line("bye")
		return true, ""
	case "/help":
		p.line("/session [id]  show or attach to a session id")
		p.line("/new           start a fresh session and attach to it")
		p.line("/reset         reset this session's context (checkpoint-flush, same session id)")
		p.line("/sessions      list live sessions")
		p.line("/exit          quit")
		return false, ""
	case "/session":
		if len(fields) < 2 {
			p.line("attached to " + id)
			return false, ""
		}
		got, _, err := c.Resolve(ctx, Target{ID: fields[1]})
		if err != nil {
			p.line("error: " + err.Error())
			return false, ""
		}
		p.line("attached to " + got + " (restart chat to stream its events)")
		return false, got
	case "/new":
		got, err := c.CreateSession(ctx, DefaultTitle(), "")
		if err != nil {
			p.line("error: " + err.Error())
			return false, ""
		}
		p.line("started " + got + " (restart chat to stream its events)")
		return false, got
	case "/reset":
		if _, err := c.do(ctx, "POST", "/sessions/"+id+"/reset", strings.NewReader(`{"reason":"explicit (cli)"}`), "application/json"); err != nil {
			p.line("error: " + err.Error())
			return false, ""
		}
		p.line("context reset (session id unchanged)")
		return false, ""
	case "/sessions":
		list, err := c.Sessions(ctx)
		if err != nil {
			p.line("error: " + err.Error())
			return false, ""
		}
		for _, s := range list {
			mark := "  "
			if s.ID == id {
				mark = "* "
			}
			p.line(fmt.Sprintf("%s%s  %s  %s  turns=%d", mark, s.ID, s.Title, s.Status, s.Turns))
		}
		return false, ""
	default:
		p.line("unknown command " + fields[0] + " (try /help)")
		return false, ""
	}
}

// turn sends one prompt and renders the turn: streamed output and tool calls as
// they arrive, a live working indicator, and the final answer (suppressed when
// it merely repeats the last streamed output, the same single-visible-reply
// rule the channels apply).
func (p *printer) turn(ctx context.Context, c *Client, id, text string, events <-chan eventbus.Event, interrupt <-chan struct{}) error {
	turnCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	type outcome struct {
		result string
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		r, err := c.Send(turnCtx, id, text)
		done <- outcome{r, err}
	}()

	p.state("working")
	p.startSpinner()
	defer p.stopSpinner()

	lastOutput := ""
	for {
		select {
		case e := <-events:
			if e.SessionID != id {
				continue
			}
			switch e.Kind {
			case eventbus.KindOutput:
				if strings.TrimSpace(e.Text) != "" {
					lastOutput = e.Text
					p.line(e.Text)
				}
			case eventbus.KindToolCall:
				p.dim("  . " + firstNonEmpty(e.Tool, "tool") + " " + oneLine(e.Text, 100))
			case eventbus.KindNeedsInput:
				p.state("needs input")
			case eventbus.KindError:
				p.dim("  ! " + oneLine(e.Text, 200))
			}
		case <-interrupt:
			p.line("")
			p.line("interrupting the turn (the session stays alive)")
			ictx, icancel := context.WithTimeout(context.Background(), 10*time.Second)
			if ierr := c.Interrupt(ictx, id); ierr != nil {
				p.line("interrupt failed: " + ierr.Error())
			}
			icancel()
			cancel()
			<-done
			p.state("interrupted")
			return nil
		case o := <-done:
			if o.err != nil {
				return o.err
			}
			p.state("done")
			if o.result != "" && o.result != lastOutput {
				p.line(o.result)
			}
			if o.result == "" && lastOutput == "" {
				p.line("(no result)")
			}
			return nil
		}
	}
}

// ---- terminal rendering ----

// printer serializes everything written to the terminal so the spinner, which
// owns a redrawn line on stderr, never garbles a printed answer.
type printer struct {
	mu       sync.Mutex
	out      io.Writer
	err      io.Writer
	tty      bool
	spinning bool
	label    string
	since    time.Time
	stop     chan struct{}
}

// line prints one answer/notice line to stdout (clearing the indicator first).
func (p *printer) line(s string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.clear()
	fmt.Fprintln(p.out, s)
}

// dim prints a secondary line (tool calls, harness errors) on stderr so stdout
// stays the answer channel even in the REPL.
func (p *printer) dim(s string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.clear()
	fmt.Fprintln(p.err, s)
}

// prompt writes the input prompt without a newline.
func (p *printer) prompt(s string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.clear()
	fmt.Fprint(p.out, s)
}

// state sets the indicator label: the CLI analogue of the Telegram reaction
// chain (working -> needs input -> done / error). On a non-tty it prints one
// bracketed line instead of animating.
func (p *printer) state(label string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.label = label
	if !p.tty {
		fmt.Fprintf(p.err, "[%s]\n", label)
		return
	}
	p.clear()
	if label == "working" || label == "needs input" {
		return // the spinner redraws it
	}
	fmt.Fprintf(p.err, "[%s]\n", label)
}

// clear erases the spinner line. Caller holds the lock.
func (p *printer) clear() {
	if p.tty && p.spinning {
		fmt.Fprint(p.err, "\r\033[K")
	}
}

func (p *printer) startSpinner() {
	if !p.tty {
		return
	}
	p.mu.Lock()
	p.spinning = true
	p.since = time.Now()
	p.stop = make(chan struct{})
	stop := p.stop
	p.mu.Unlock()
	go func() {
		frames := []rune{'|', '/', '-', '\\'}
		t := time.NewTicker(120 * time.Millisecond)
		defer t.Stop()
		i := 0
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				p.mu.Lock()
				if p.spinning {
					fmt.Fprintf(p.err, "\r\033[K%c %s %ds", frames[i%len(frames)], p.label, int(time.Since(p.since).Seconds()))
				}
				p.mu.Unlock()
				i++
			}
		}
	}()
}

func (p *printer) stopSpinner() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.spinning {
		return
	}
	close(p.stop)
	fmt.Fprint(p.err, "\r\033[K")
	p.spinning = false
}

// isTTY reports whether w is an interactive terminal (stdlib only: a character
// device, which a pipe or a file is not).
func isTTY(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	st, err := f.Stat()
	if err != nil {
		return false
	}
	return st.Mode()&os.ModeCharDevice != 0
}

// oneLine flattens and caps a string for a single status line.
func oneLine(s string, max int) string {
	s = strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(s, "\n", " "), "\r", " "))
	r := []rune(s)
	if len(r) > max {
		return string(r[:max]) + "..."
	}
	return s
}

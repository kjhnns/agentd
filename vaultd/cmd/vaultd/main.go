// Command vaultd is the password exchange service between Joe's iPhone (the
// Vault tab of the Agent app) and the pass store on the box. Spec:
// docs/specs/2026-10-vault-protocol.md.
//
//	vaultd serve [-config ~/.config/vaultd/config.json]
//	vaultd pair-start [--replace]
//	vaultd devices | unpair <dev> | reset-keys --yes | list | status
//	vaultd generate --pass-path P --label L [--login U] [--url X] [--length 24] [--no-symbols] [--force]
//	vaultd share --pass-path P --label L
//	vaultd exec [--output redacted] <item> -- /abs/cmd [args...]
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/kjhnns/agentd/vaultd/internal/passstore"
	"github.com/kjhnns/agentd/vaultd/internal/vault"
)

// fileConfig is ~/.config/vaultd/config.json (mode 0600).
type fileConfig struct {
	ServerID        string   `json:"server_id"`
	PublicAddr      string   `json:"public_addr"`
	PairAddr        string   `json:"pair_addr"`
	AdminSock       string   `json:"admin_sock"`
	StateDir        string   `json:"state_dir"`
	StoreRoot       string   `json:"store_root"`
	RouteToken      string   `json:"route_token"` // "pass:<path>" or "env:<NAME>"
	PairLogin       string   `json:"pair_login"`
	PairOS          string   `json:"pair_os"`
	MaxDevices      int      `json:"max_devices"`
	BackupExclusion string   `json:"backup_exclusion"`
	NotifyCmd       []string `json:"notify_cmd"`
	TestMode        bool     `json:"test_mode"`
}

func home() string { h, _ := os.UserHomeDir(); return h }

func defaultSock() string {
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		return filepath.Join(d, "vaultd", "admin.sock")
	}
	return filepath.Join("/run/user", fmt.Sprint(os.Getuid()), "vaultd", "admin.sock")
}

func expand(p string) string {
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home(), p[2:])
	}
	return p
}

func loadConfig(path string) (vault.Config, error) {
	var fc fileConfig
	fi, err := os.Stat(path)
	if err != nil {
		return vault.Config{}, err
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return vault.Config{}, fmt.Errorf("%s must be mode 0600", path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return vault.Config{}, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&fc); err != nil {
		return vault.Config{}, err
	}
	cfg := vault.Config{ServerID: fc.ServerID, PublicAddr: fc.PublicAddr, PairAddr: fc.PairAddr,
		AdminSock: expand(fc.AdminSock), StateDir: expand(fc.StateDir), StoreRoot: expand(fc.StoreRoot),
		PairLogin: fc.PairLogin, PairOS: fc.PairOS, MaxDevices: fc.MaxDevices,
		BackupExclusion: fc.BackupExclusion, NotifyCmd: fc.NotifyCmd, TestMode: fc.TestMode}
	if cfg.AdminSock == "" {
		cfg.AdminSock = defaultSock()
	}
	if cfg.StateDir == "" {
		cfg.StateDir = filepath.Join(home(), ".local", "state", "vaultd")
	}
	defStore := filepath.Join(home(), ".password-store")
	if cfg.StoreRoot == "" {
		cfg.StoreRoot = defStore
	}
	if !cfg.TestMode && filepath.Clean(cfg.StoreRoot) != defStore {
		return vault.Config{}, errors.New("store_root may differ from ~/.password-store only with test_mode (spec 12)")
	}
	if cfg.PairOS == "" {
		cfg.PairOS = "iOS"
	}
	if cfg.PublicAddr == "" || cfg.PairAddr == "" || cfg.PairLogin == "" {
		return vault.Config{}, errors.New("public_addr, pair_addr and pair_login are required")
	}
	tok, err := resolveSecret(fc.RouteToken)
	if err != nil {
		return vault.Config{}, fmt.Errorf("route_token: %w", err)
	}
	cfg.RouteToken = tok
	return cfg, nil
}

func resolveSecret(ref string) (string, error) {
	switch {
	case strings.HasPrefix(ref, "env:"):
		v := os.Getenv(strings.TrimPrefix(ref, "env:"))
		if v == "" {
			return "", errors.New("env variable empty")
		}
		return v, nil
	case strings.HasPrefix(ref, "pass:"):
		p := strings.TrimPrefix(ref, "pass:")
		if err := passstore.ValidPath(p); err != nil {
			return "", err
		}
		out, err := exec.Command("pass", "show", "--", p).Output()
		if err != nil {
			return "", errors.New("pass show failed")
		}
		return strings.SplitN(string(out), "\n", 2)[0], nil
	}
	return "", errors.New(`use "pass:<path>" or "env:<NAME>"`)
}

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)
	if len(os.Args) < 2 {
		usage()
	}
	cmd, args := os.Args[1], os.Args[2:]
	switch cmd {
	case "serve":
		serve(args)
	case "exec":
		os.Exit(runExec(args))
	default:
		os.Exit(client(cmd, args))
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: vaultd serve|pair-start|devices|unpair|reset-keys|generate|share|list|exec|status")
	os.Exit(2)
}

func serve(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	cfgPath := fs.String("config", filepath.Join(home(), ".config", "vaultd", "config.json"), "config file (0600)")
	_ = fs.Parse(args)
	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		log.Fatalf("vaultd: config: %v", err)
	}
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		log.Fatalf("vaultd: %v", err)
	}
	// One daemon per state dir.
	lf, err := os.OpenFile(filepath.Join(cfg.StateDir, "lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		log.Fatalf("vaultd: lock: %v", err)
	}
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		log.Fatalf("vaultd: another vaultd runs on %s", cfg.StateDir)
	}
	// The lock lives as long as lf: keep it referenced until Serve returns
	// (a collected *os.File would close the fd and drop the lock).
	defer lf.Close()
	store := &passstore.GPG{Root: cfg.StoreRoot}
	s, err := vault.New(cfg, store, vault.TailscaleWhois)
	if err != nil {
		log.Fatalf("vaultd: %v", err)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	if err := s.Serve(ctx); err != nil {
		log.Fatalf("vaultd: %v", err)
	}
	runtime.KeepAlive(lf)
}

func call(req vault.AdminRequest) (vault.AdminResponse, error) {
	sock := os.Getenv("VAULTD_SOCK")
	if sock == "" {
		sock = defaultSock()
	}
	c, err := net.DialTimeout("unix", sock, 5*time.Second)
	if err != nil {
		return vault.AdminResponse{}, fmt.Errorf("vaultd not reachable at %s", sock)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(90 * time.Second))
	b, _ := json.Marshal(req)
	if _, err := c.Write(append(b, '\n')); err != nil {
		return vault.AdminResponse{}, err
	}
	line, err := bufio.NewReader(c).ReadBytes('\n')
	if err != nil {
		return vault.AdminResponse{}, err
	}
	var r vault.AdminResponse
	return r, json.Unmarshal(line, &r)
}

func client(cmd string, args []string) int {
	req := vault.AdminRequest{Cmd: cmd}
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	switch cmd {
	case "pair-start":
		fs.BoolVar(&req.Replace, "replace", false, "revoke the paired device when the new one completes")
	case "reset-keys":
		yes := fs.Bool("yes", false, "confirm")
		_ = fs.Parse(args)
		if !*yes {
			fmt.Fprintln(os.Stderr, "reset-keys revokes every device; pass --yes")
			return 2
		}
	case "generate":
		fs.StringVar(&req.Path, "pass-path", "", "pass path (not under vault/)")
		fs.StringVar(&req.Label, "label", "", "label shown in the app")
		fs.StringVar(&req.Login, "login", "", "login")
		fs.StringVar(&req.URL, "url", "", "url")
		fs.IntVar(&req.Length, "length", 24, "12..128")
		fs.BoolVar(&req.NoSym, "no-symbols", false, "letters and digits only")
		fs.BoolVar(&req.Force, "force", false, "overwrite an existing entry")
	case "share":
		fs.StringVar(&req.Path, "pass-path", "", "existing pass path")
		fs.StringVar(&req.Label, "label", "", "label shown in the app")
	case "unpair":
		if len(args) != 1 {
			fmt.Fprintln(os.Stderr, "usage: vaultd unpair <dev>")
			return 2
		}
		req.Dev = args[0]
		args = nil
	case "devices", "list", "status":
	default:
		usage()
	}
	if cmd != "reset-keys" {
		_ = fs.Parse(args)
	}
	r, err := call(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, "vaultd:", err)
		return 1
	}
	if !r.OK {
		fmt.Fprintln(os.Stderr, "vaultd:", r.Err)
		return 1
	}
	fmt.Print(strings.TrimRight(r.Out, "\n") + "\n")
	return 0
}

// runExec hands the value of an item to a child's stdin (spec section 14).
func runExec(args []string) int {
	fs := flag.NewFlagSet("exec", flag.ExitOnError)
	output := fs.String("output", "none", `"none" (default) or "redacted" (best effort)`)
	_ = fs.Parse(args)
	rest := fs.Args()
	if len(rest) < 3 || rest[1] != "--" || !filepath.IsAbs(rest[2]) {
		fmt.Fprintln(os.Stderr, "usage: vaultd exec [--output redacted] <item> -- /abs/cmd [args...]")
		return 2
	}
	if *output != "none" && *output != "redacted" {
		fmt.Fprintln(os.Stderr, "vaultd: --output must be none or redacted")
		return 2
	}
	r, err := call(vault.AdminRequest{Cmd: "secret", Item: rest[0]})
	if err != nil || !r.OK {
		msg := r.Err
		if err != nil {
			msg = err.Error()
		}
		fmt.Fprintln(os.Stderr, "vaultd:", msg)
		return 1
	}
	secret := []byte(r.Secret)
	r.Secret = ""
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, rest[2], rest[3:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.Stdin = bytes.NewReader(append(secret, '\n'))
	var buf capBuf
	if *output == "redacted" {
		cmd.Stdout, cmd.Stderr = &buf, &buf
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		<-sig
		cancel()
	}()
	cmd.WaitDelay = 5 * time.Second
	err = cmd.Run()
	if cmd.Process != nil {
		// Descendants that outlive the leader die with the group.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	if *output == "redacted" {
		out := passstore.RedactAll(buf.b.Bytes(), [][]byte{secret})
		_, _ = os.Stdout.Write(out)
		if buf.over {
			fmt.Fprintln(os.Stderr, "vaultd: output truncated at 64 KiB")
		}
	}
	for i := range secret {
		secret[i] = 0
	}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode()
		}
		fmt.Fprintln(os.Stderr, "vaultd: exec failed")
		return 1
	}
	return 0
}

type capBuf struct {
	b    bytes.Buffer
	over bool
}

func (c *capBuf) Write(p []byte) (int, error) {
	if c.b.Len()+len(p) > 64<<10 {
		c.over = true
		return len(p), nil
	}
	return c.b.Write(p)
}

var _ io.Writer = (*capBuf)(nil)

// Package passstore reads and writes the pass file layout for vaultd (spec
// section 13). It does not run the pass script: writes go through gpg into a
// temp file that is fsynced and renamed, so a crash never leaves a half-written
// entry and a recorded "done" is durable. `pass show` keeps working on every
// entry it writes.
package passstore

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/kjhnns/agentd/vaultd/internal/vcrypto"
	"github.com/kjhnns/agentd/vaultd/internal/wire"
)

// Store is what vaultd needs from the password store.
type Store interface {
	Write(ctx context.Context, path string, content []byte) error
	Read(ctx context.Context, path string) ([]byte, error)
	Remove(path string) (bool, error) // true = the entry is absent and durable afterwards
	CleanTemps() (int, error)
	Exists(path string) (bool, error)
}

// ErrNotFound means the entry does not exist.
var ErrNotFound = errors.New("entry not found")

// ErrUncertain means the new file may already be in place but its
// durability is not established (a failure after the rename). The caller
// must retry the same write; it must not report failure.
var ErrUncertain = errors.New("write outcome uncertain")

// TempPrefix names every temp file the adapter creates, so a start-up sweep
// can find leftovers of a crash.
const TempPrefix = ".vaultd-tmp-"

// MaxPlain caps decrypted output (an entry is at most a few KiB).
const MaxPlain = 64 << 10

var segRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]*$`)

// ValidPath enforces the path rule of spec section 13.
func ValidPath(p string) error {
	if len(p) == 0 || len(p) > 200 {
		return errors.New("invalid path")
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "." || seg == ".." || !segRe.MatchString(seg) {
			return errors.New("invalid path")
		}
	}
	return nil
}

// ValidAgentPath is ValidPath plus: the first segment is not "vault" (the
// reserved subtree of given items).
func ValidAgentPath(p string) error {
	if err := ValidPath(p); err != nil {
		return err
	}
	if strings.Split(p, "/")[0] == "vault" {
		return errors.New("path is in the reserved vault/ subtree")
	}
	return nil
}

// GPG is the store adapter over the gpg binary.
type GPG struct {
	Root    string // store root (production: ~/.password-store)
	Bin     string // default /usr/bin/gpg
	Home    string // GNUPGHOME for the child, "" = inherit the default
	Timeout time.Duration
}

func (g *GPG) bin() string {
	if g.Bin != "" {
		return g.Bin
	}
	return "/usr/bin/gpg"
}

func (g *GPG) env() []string {
	home, _ := os.UserHomeDir()
	env := []string{"PATH=/usr/bin:/bin", "HOME=" + home, "LANG=C"}
	if g.Home != "" {
		env = append(env, "GNUPGHOME="+g.Home)
	} else if v := os.Getenv("GNUPGHOME"); v != "" {
		env = append(env, "GNUPGHOME="+v)
	}
	return env
}

func (g *GPG) run(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	t := g.Timeout
	if t == 0 {
		t = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, t)
	defer cancel()
	cmd := exec.CommandContext(ctx, g.bin(), args...)
	cmd.Env = g.env()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	out := &capped{max: MaxPlain}
	cmd.Stdout = out
	cmd.Stderr = nil // never captured: it may echo content
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	cmd.WaitDelay = 5 * time.Second
	err := cmd.Run()
	if cmd.Process != nil {
		// Leftover descendants of the leader die with the group.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	if err == nil && out.over {
		err = errors.New("output too large")
	}
	if err != nil {
		wipeBytes(out.b.Bytes())
		code := -1
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		}
		return nil, fmt.Errorf("gpg failed, exit %d", code)
	}
	return out.b.Bytes(), nil
}

// capped buffers up to max bytes and drops the rest (the run then fails).
type capped struct {
	b    bytes.Buffer
	max  int
	over bool
}

func (c *capped) Write(p []byte) (int, error) {
	if c.b.Len()+len(p) > c.max {
		c.over = true
		return len(p), nil
	}
	return c.b.Write(p)
}

func wipeBytes(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// CheckConfig fails closed on pass features the adapter does not implement
// (spec section 13): recipient override and signed .gpg-id files.
func CheckConfig() error {
	for _, v := range []string{"PASSWORD_STORE_KEY", "PASSWORD_STORE_SIGNING_KEY", "PASSWORD_STORE_GPG_OPTS"} {
		if os.Getenv(v) != "" {
			return fmt.Errorf("%s is set; vaultd does not support it", v)
		}
	}
	return nil
}

// checkDirs verifies every directory from the root down to path's directory
// is a real directory or absent (absent only when allowMissing).
func (g *GPG) checkDirs(path string, allowMissing bool) error {
	fi, err := os.Lstat(g.Root)
	if err != nil || !fi.IsDir() {
		return errors.New("store root is not a directory")
	}
	segs := strings.Split(path, "/")
	cur := g.Root
	for _, s := range segs[:len(segs)-1] {
		cur = filepath.Join(cur, s)
		fi, err := os.Lstat(cur)
		if errors.Is(err, os.ErrNotExist) && allowMissing {
			return nil
		}
		if err != nil {
			return err
		}
		if !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
			return errors.New("path component is not a directory")
		}
	}
	return nil
}

func (g *GPG) file(path string) string { return filepath.Join(g.Root, filepath.FromSlash(path)+".gpg") }

// Exists reports whether the entry is a regular file. Anything else at that
// name (symlink, directory) is an error.
func (g *GPG) Exists(path string) (bool, error) {
	if err := ValidPath(path); err != nil {
		return false, err
	}
	if err := g.checkDirs(path, true); err != nil {
		return false, err
	}
	fi, err := os.Lstat(g.file(path))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !fi.Mode().IsRegular() {
		return false, errors.New("entry is not a regular file")
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && st.Nlink > 1 {
		return false, errors.New("entry has more than one hard link")
	}
	return true, nil
}

// recipients finds the nearest .gpg-id from the entry's directory up to the
// root (pass's rule: inline "#" comments stripped). The file is bounded
// (4 KiB, 16 recipients); a signed .gpg-id or any lookup error fails closed.
func (g *GPG) recipients(path string) ([]string, error) {
	dir := filepath.Dir(g.file(path))
	for {
		f := filepath.Join(dir, ".gpg-id")
		fi, err := os.Lstat(f)
		if err == nil {
			if !fi.Mode().IsRegular() || fi.Size() > 4096 {
				return nil, errors.New(".gpg-id is not a small regular file")
			}
			if _, serr := os.Lstat(f + ".sig"); serr == nil || !errors.Is(serr, os.ErrNotExist) {
				return nil, errors.New("signed .gpg-id is not supported")
			}
			b, err := os.ReadFile(f)
			if err != nil {
				return nil, err
			}
			var ids []string
			sc := bufio.NewScanner(bytes.NewReader(b))
			for sc.Scan() {
				l := sc.Text()
				if i := strings.IndexByte(l, '#'); i >= 0 {
					l = l[:i]
				}
				if l = strings.TrimSpace(l); l != "" {
					ids = append(ids, l)
				}
			}
			if err := sc.Err(); err != nil {
				return nil, err
			}
			if len(ids) == 0 || len(ids) > 16 {
				return nil, errors.New(".gpg-id must name 1 to 16 recipients")
			}
			return ids, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		if filepath.Clean(dir) == filepath.Clean(g.Root) {
			return nil, errors.New("no .gpg-id in the store")
		}
		dir = filepath.Dir(dir)
	}
}

func fsyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// mkdirs creates the missing directories of path (0700) and fsyncs every
// parent on the way, also for directories a failed earlier attempt left, so
// the whole chain is durable before a write is acknowledged.
func (g *GPG) mkdirs(path string) error {
	segs := strings.Split(path, "/")
	cur := g.Root
	for _, s := range segs[:len(segs)-1] {
		next := filepath.Join(cur, s)
		if err := os.Mkdir(next, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
		if err := fsyncDir(cur); err != nil {
			return err
		}
		cur = next
	}
	return g.checkDirs(path, false)
}

// Write encrypts content to the entry: temp file, fsync, rename, fsync dir.
// On any failure the old entry is untouched.
func (g *GPG) Write(ctx context.Context, path string, content []byte) error {
	if err := ValidPath(path); err != nil {
		return err
	}
	if _, err := g.Exists(path); err != nil {
		return err
	}
	if err := g.mkdirs(path); err != nil {
		return err
	}
	ids, err := g.recipients(path)
	if err != nil {
		return err
	}
	dir := filepath.Dir(g.file(path))
	tmp := filepath.Join(dir, TempPrefix+vcrypto.RandomID())
	defer os.Remove(tmp)
	args := []string{"--batch", "--quiet", "--yes", "--compress-algo=none", "--no-encrypt-to", "-e"}
	for _, id := range ids {
		args = append(args, "-r", id)
	}
	args = append(args, "-o", tmp)
	if _, err := g.run(ctx, content, args...); err != nil {
		return err
	}
	f, err := os.OpenFile(tmp, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	fi, err := f.Stat()
	if err == nil && fi.Size() == 0 {
		err = errors.New("gpg wrote an empty file")
	}
	if err == nil {
		err = f.Chmod(0o600)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if err := os.Rename(tmp, g.file(path)); err != nil {
		// rename(2) is atomic: on error the old entry is still in place.
		return err
	}
	if err := fsyncDir(dir); err != nil {
		return ErrUncertain
	}
	return nil
}

// Read decrypts the entry into memory.
func (g *GPG) Read(ctx context.Context, path string) ([]byte, error) {
	ok, err := g.Exists(path)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrNotFound
	}
	return g.run(ctx, nil, "--batch", "--quiet", "--decrypt", g.file(path))
}

// Remove deletes the entry and fsyncs its directory (or its nearest
// existing ancestor) even when the entry was already absent, so "absent" is
// only reported once it is durable.
func (g *GPG) Remove(path string) (bool, error) {
	ok, err := g.Exists(path)
	if err != nil {
		return false, err
	}
	if ok {
		if err := os.Remove(g.file(path)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
	}
	dir := filepath.Dir(g.file(path))
	for {
		if fi, err := os.Lstat(dir); err == nil && fi.IsDir() {
			break
		}
		if filepath.Clean(dir) == filepath.Clean(g.Root) {
			return false, errors.New("store root missing")
		}
		dir = filepath.Dir(dir)
	}
	if err := fsyncDir(dir); err != nil {
		return false, err
	}
	ok, err = g.Exists(path)
	if err != nil {
		return false, err
	}
	return !ok, nil
}

// CleanTemps removes leftover adapter temp files anywhere in the store and
// fsyncs EVERY directory it visits, also where it removed nothing now: an
// earlier run may have unlinked a file and failed its fsync (code r2 #36).
// Any error fails the sweep (vaultd then refuses to start).
func (g *GPG) CleanTemps() (int, error) {
	n := 0
	err := filepath.WalkDir(g.Root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(d.Name(), TempPrefix) {
			if err := os.Remove(p); err != nil {
				return err
			}
			n++
		}
		return nil
	})
	if err != nil {
		return n, err
	}
	err = filepath.WalkDir(g.Root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return fsyncDir(p)
		}
		return nil
	})
	return n, err
}

// Entry is a parsed pass entry (spec section 13, shared import).
type Entry struct {
	Password string
	Login    string
	URL      string
}

// ParseEntry reads the shared import format (spec 13, R19 as amended in
// R21): line 1 is the password exactly (no character is stripped; a control
// character, including a trailing CR, makes the entry unsupported). Later
// lines "login: ", "user: ", "username: ", "email: " (login) and "url: "
// (url), exact lowercase keys; the first line of each field wins, even when
// empty; an invalid first value makes the entry unsupported. Other lines are
// ignored and never output.
func ParseEntry(b []byte) (Entry, error) {
	unsupported := errors.New("entry format not supported")
	lines := strings.Split(string(b), "\n")
	var e Entry
	e.Password = lines[0]
	if len(e.Password) == 0 || len(e.Password) > 1024 || hasControl(e.Password) {
		return Entry{}, unsupported
	}
	var seenLogin, seenURL bool
	for _, l := range lines[1:] {
		k, v, ok := strings.Cut(l, ": ")
		if !ok {
			continue
		}
		switch k {
		case "login", "user", "username", "email":
			if seenLogin {
				continue
			}
			seenLogin = true
			if len(v) > 200 || hasControl(v) {
				return Entry{}, unsupported
			}
			e.Login = v
		case "url":
			if seenURL {
				continue
			}
			seenURL = true
			if len(v) > 500 || hasControl(v) {
				return Entry{}, unsupported
			}
			e.URL = v
		}
	}
	return e, nil
}

// FormatEntry writes the vault entry layout (spec section 7).
func FormatEntry(password, login, url, label string) []byte {
	return []byte(password + "\nlogin: " + login + "\nurl: " + url + "\nlabel: " + label + "\n")
}

func hasControl(s string) bool { return wire.HasControl(s) }

// RedactAll replaces every exact occurrence of each needle with [redacted],
// longest needle first. Best effort only (spec section 14): a writer that
// encodes or splits the value defeats it.
func RedactAll(b []byte, needles [][]byte) []byte {
	out := append([]byte(nil), b...)
	sorted := append([][]byte(nil), needles...)
	for i := range sorted {
		for j := i + 1; j < len(sorted); j++ {
			if len(sorted[j]) > len(sorted[i]) {
				sorted[i], sorted[j] = sorted[j], sorted[i]
			}
		}
	}
	for _, n := range sorted {
		if len(n) > 0 {
			out = bytes.ReplaceAll(out, n, []byte("[redacted]"))
		}
	}
	return out
}

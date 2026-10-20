package passstore

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidPath(t *testing.T) {
	for _, p := range []string{"ebay/password", "vault/given/0123abcd", "a.b_c-d"} {
		if err := ValidPath(p); err != nil {
			t.Errorf("%s rejected", p)
		}
	}
	for _, p := range []string{"", "/abs", "a/../b", "..", ".", "a//b", "a/", "-rf", "a/-x", "a b", "a;b", "a/./b", "x\ny", ".hidden", "a/.b"} {
		if err := ValidPath(p); err == nil {
			t.Errorf("%q accepted", p)
		}
	}
	if err := ValidAgentPath("vault/given/x"); err == nil {
		t.Error("vault/ accepted for an agent path")
	}
	if err := ValidAgentPath("ibkr/password"); err != nil {
		t.Error(err)
	}
}

func TestParseEntry(t *testing.T) {
	e, err := ParseEntry([]byte("pw1\nusername: joe\nurl: https://x\nnotes: secret stuff\n"))
	if err != nil || e.Password != "pw1" || e.Login != "joe" || e.URL != "https://x" {
		t.Fatalf("%v %+v", err, e)
	}
	if _, err := ParseEntry([]byte("\nlogin: x")); err == nil {
		t.Fatal("empty password accepted")
	}
}

// NewTestStore builds a store with a throwaway GPG key (no passphrase) in a
// temp dir. It returns the adapter and the env for running the real pass.
func NewTestStore(t *testing.T) (*GPG, []string) {
	t.Helper()
	for _, b := range []string{"pass", "/usr/bin/gpg"} {
		if _, err := exec.LookPath(b); err != nil {
			t.Skipf("%s not installed", b)
		}
	}
	dir := t.TempDir()
	gh := filepath.Join(dir, "gnupg")
	if err := os.Mkdir(gh, 0o700); err != nil {
		t.Fatal(err)
	}
	store := filepath.Join(dir, "store")
	env := []string{"GNUPGHOME=" + gh, "PASSWORD_STORE_DIR=" + store}
	gen := exec.Command("/usr/bin/gpg", "--batch", "--passphrase", "", "--quick-gen-key", "vaultd-test@example.invalid", "default", "default", "never")
	gen.Env = append(os.Environ(), env...)
	if out, err := gen.CombinedOutput(); err != nil {
		t.Fatalf("gpg gen: %v %s", err, out)
	}
	ini := exec.Command("pass", "init", "vaultd-test@example.invalid")
	ini.Env = append(os.Environ(), env...)
	if out, err := ini.CombinedOutput(); err != nil {
		t.Fatalf("pass init: %v %s", err, out)
	}
	t.Cleanup(func() {
		k := exec.Command("gpgconf", "--kill", "gpg-agent")
		k.Env = append(os.Environ(), env...)
		_ = k.Run()
	})
	return &GPG{Root: store, Home: gh}, env
}

func TestRoundTripAndPassCompat(t *testing.T) {
	g, env := NewTestStore(t)
	ctx := context.Background()
	content := FormatEntry("s3cr3t-value", "joe", "https://x", "Label")
	if err := g.Write(ctx, "vault/given/abc", content); err != nil {
		t.Fatal(err)
	}
	got, err := g.Read(ctx, "vault/given/abc")
	if err != nil || string(got) != string(content) {
		t.Fatalf("read: %v %q", err, got)
	}
	show := exec.Command("pass", "show", "vault/given/abc")
	show.Env = append(os.Environ(), env...)
	out, err := show.Output()
	if err != nil || string(out) != string(content) {
		t.Fatalf("pass show: %v %q", err, out)
	}
	if err := g.Write(ctx, "vault/given/abc", []byte("new\n")); err != nil {
		t.Fatal(err)
	}
	got, _ = g.Read(ctx, "vault/given/abc")
	if string(got) != "new\n" {
		t.Fatalf("replace: %q", got)
	}
	ents, _ := os.ReadDir(filepath.Join(g.Root, "vault", "given"))
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), TempPrefix) {
			t.Fatalf("temp file left: %s", e.Name())
		}
	}
	gone, err := g.Remove("vault/given/abc")
	if err != nil || !gone {
		t.Fatalf("remove: %v %v", err, gone)
	}
	gone, err = g.Remove("vault/given/abc")
	if err != nil || !gone {
		t.Fatalf("remove of an absent entry must succeed: %v %v", err, gone)
	}
	if _, err := g.Read(ctx, "vault/given/abc"); err != ErrNotFound {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestSymlinkRefused(t *testing.T) {
	g, _ := NewTestStore(t)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(g.Root, "vault")); err != nil {
		t.Fatal(err)
	}
	if err := g.Write(context.Background(), "vault/given/abc", []byte("x\n")); err == nil {
		t.Fatal("write through a symlinked dir accepted")
	}
	if err := os.Symlink("/etc/hostname", filepath.Join(g.Root, "link.gpg")); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Exists("link"); err == nil {
		t.Fatal("symlinked entry accepted")
	}
}

func TestBadRecipientLeavesOldEntry(t *testing.T) {
	g, _ := NewTestStore(t)
	ctx := context.Background()
	if err := g.Write(ctx, "a/b", []byte("old\n")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(g.Root, "a", ".gpg-id"), []byte("nobody@example.invalid\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := g.Write(ctx, "a/b", []byte("new\n")); err == nil {
		t.Fatal("write with an unknown recipient succeeded")
	}
	os.Remove(filepath.Join(g.Root, "a", ".gpg-id"))
	got, err := g.Read(ctx, "a/b")
	if err != nil || string(got) != "old\n" {
		t.Fatalf("old entry damaged: %v %q", err, got)
	}
}

func TestParseEntryRules(t *testing.T) {
	cases := map[string]bool{ // content -> accepted
		"pw\nlogin: a\nlogin: b\n":        true,
		"pw\r\nlogin: a\n":                false, // CR in the password
		"pw\nlogin: \x01bad\nlogin: ok\n": false, // first login invalid
		"pw\nLogin: x\n":                  true,  // not a known key, ignored
	}
	for in, ok := range cases {
		_, err := ParseEntry([]byte(in))
		if (err == nil) != ok {
			t.Errorf("%q: err=%v", in, err)
		}
	}
	e, _ := ParseEntry([]byte("pw\nurl: \nurl: later\n"))
	if e.URL != "" {
		t.Errorf("first url line must win even when empty, got %q", e.URL)
	}
	e, _ = ParseEntry([]byte("pw\nlogin: a\nlogin: b\n"))
	if e.Login != "a" {
		t.Errorf("first login wins, got %q", e.Login)
	}
}

func TestStaleTempsRemoved(t *testing.T) {
	g, _ := NewTestStore(t)
	p := filepath.Join(g.Root, TempPrefix+"abc")
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	n, err := g.CleanTemps()
	if err != nil || n != 1 {
		t.Fatalf("clean: %d %v", n, err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatal("temp still there")
	}
}

// Package testutil builds a throwaway GPG key and pass store for tests.
package testutil

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/kjhnns/agentd/vaultd/internal/passstore"
)

// Store returns a store adapter over a temp store with a no-passphrase key.
func Store(t testing.TB) (*passstore.GPG, []string) {
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
	return &passstore.GPG{Root: store, Home: gh}, env
}

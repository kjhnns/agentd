// Command vaultd-testpeer is a TEST-ONLY vault server for the Swift client's
// end-to-end tests: the real vault.Server with an in-memory store, a fake
// tailnet identity check and loopback listeners. It never touches pass.
//
//	vaultd-testpeer -dir /tmp/peer   (writes uri.txt, shared.txt into -dir)
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/kjhnns/agentd/vaultd/internal/passstore"
	"github.com/kjhnns/agentd/vaultd/internal/vault"
)

type memStore struct {
	mu sync.Mutex
	m  map[string][]byte
}

func (s *memStore) Write(_ context.Context, p string, c []byte) error {
	if err := passstore.ValidPath(p); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[p] = append([]byte(nil), c...)
	return nil
}

func (s *memStore) Read(_ context.Context, p string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.m[p]
	if !ok {
		return nil, passstore.ErrNotFound
	}
	return append([]byte(nil), c...), nil
}

func (s *memStore) Remove(p string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, p)
	return true, nil
}

func (s *memStore) Exists(p string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.m[p]
	return ok, nil
}

func (s *memStore) CleanTemps() (int, error) { return 0, nil }

func main() {
	dir := flag.String("dir", "", "output dir for uri.txt and shared.txt")
	port := flag.Int("port", 28790, "public port; pairing is port+1 (loopback only)")
	flag.Parse()
	if *dir == "" {
		log.Fatal("-dir is required")
	}
	if err := os.MkdirAll(*dir, 0o700); err != nil {
		log.Fatal(err)
	}
	cfg := vault.Config{ServerID: "agent.gojoe.run", PublicAddr: fmt.Sprintf("127.0.0.1:%d", *port), PairAddr: fmt.Sprintf("127.0.0.1:%d", *port+1),
		AdminSock: filepath.Join(*dir, "admin.sock"), StateDir: filepath.Join(*dir, "state"),
		RouteToken: strings.Repeat("t", 40), PairLogin: "test@example.invalid", PairOS: "iOS", MaxDevices: 1,
		BackupExclusion: "testpeer", TestMode: true}
	store := &memStore{m: map[string][]byte{}}
	who := func(context.Context, string) (string, string, string, error) {
		return "test@example.invalid", "iOS", "testnode", nil
	}
	s, err := vault.New(cfg, store, who)
	if err != nil {
		log.Fatal(err)
	}
	_ = store.Write(context.Background(), "site/password", []byte("shared-pw-123\nlogin: joe\nurl: https://site.example\n"))
	id, err := s.Share(context.Background(), vault.AdminRequest{Path: "site/password", Label: "Site"})
	if err != nil {
		log.Fatal(err)
	}
	uri, err := s.PairStart(false)
	if err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(*dir, "uri.txt"), []byte(uri), 0o600); err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(*dir, "shared.txt"), []byte(id), 0o600); err != nil {
		log.Fatal(err)
	}
	s.TestReadFloorAtStart()
	log.Printf("testpeer ready: public %s, pairing %s", cfg.PublicAddr, cfg.PairAddr)
	if err := s.Serve(context.Background()); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

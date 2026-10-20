// Package vault is the vaultd daemon: state, journal, public and pairing
// handlers, admin socket (spec docs/specs/2026-10-vault-protocol.md).
package vault

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"

	"github.com/kjhnns/agentd/vaultd/internal/vcrypto"
)

// Device is a paired phone.
type Device struct {
	Dev      string `json:"dev"`
	Pkid     string `json:"pkid"`
	SigPub   []byte `json:"sig_pub"`
	AgrPub   []byte `json:"agr_pub"`
	Name     string `json:"name"`
	Gen      string `json:"gen"`
	PairedAt int64  `json:"paired_at"`
	Revoked  bool   `json:"revoked"`
}

// Item kinds and statuses.
const (
	KindGiven  = "given"
	KindShared = "shared"

	StatusPending    = "pending"
	StatusLive       = "live"
	StatusTombstoned = "tombstoned"
)

// Item is a given or shared item.
type Item struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	Status   string `json:"status"`
	Version  int64  `json:"version"`
	Created  int64  `json:"created"`
	Updated  int64  `json:"updated"`
	LastUse  int64  `json:"last_use"`
	PassPath string `json:"pass_path"`
	Busy     string `json:"busy,omitempty"` // rid of the op applying to this item
	// Metadata: given items get it from the phone payload, shared items from
	// the agent (label) and the entry (login, url). Never logged.
	Label string `json:"label"`
	Login string `json:"login"`
	URL   string `json:"url"`
}

// Op states.
const (
	StateApplying = "applying"
	StateDone     = "done"
	StateFailed   = "failed"
)

// Op is a journaled mutation. Header holds the exact signed request header
// (with the ciphertext) while the op is applying; it is dropped when the op
// ends.
type Op struct {
	Rid        string `json:"rid"`
	Dev        string `json:"dev"`
	Gen        string `json:"gen"`
	Rh         []byte `json:"rh"`
	Header     []byte `json:"header,omitempty"`
	Op         string `json:"op"`
	Item       string `json:"item"`
	Iv         int64  `json:"iv"`
	State      string `json:"state"`
	Code       string `json:"code,omitempty"`
	Validation bool   `json:"validation,omitempty"` // failed in validation, no side effect
	Uncertain  bool   `json:"uncertain,omitempty"`  // a write may already be in place: never "failed" from a later error
	ResultIv   int64  `json:"result_iv,omitempty"`  // the item version this op produced
	// Generate ops (admin): the pass path, the new metadata (published on
	// done) and the entry content sealed to the box intake key, so the
	// retry runner can finish the same value after a crash (code r2 #31).
	Path     string        `json:"path,omitempty"`
	Meta     *GenerateMeta `json:"meta,omitempty"`
	Blob     []byte        `json:"blob,omitempty"`
	Created  int64         `json:"created"`
	Finished int64         `json:"finished,omitempty"`
	Response []byte        `json:"response,omitempty"` // signed response bytes (done/failed)
}

// GenerateMeta is the metadata a generate op publishes when it is done.
type GenerateMeta struct {
	Label string `json:"label"`
	Login string `json:"login"`
	URL   string `json:"url"`
}

// ReadRec remembers a read request id for the replay check.
type ReadRec struct {
	Dev string `json:"dev"`
	Rh  []byte `json:"rh"`
	Ts  int64  `json:"ts"`
}

// Pairing states.
const (
	PairOpen     = "open"
	PairAnswered = "answered"
	PairDone     = "done"
)

// Pairing is the single pairing record.
type Pairing struct {
	Pid      string `json:"pid"`
	S        []byte `json:"s,omitempty"`
	Expiry   int64  `json:"expiry"`
	Attempts int    `json:"attempts"`
	State    string `json:"state"`
	Bsid     string `json:"bsid"`
	Bkid     string `json:"bkid"`
	Replace  bool   `json:"replace"`
	Node     string `json:"node,omitempty"`
	TH       []byte `json:"th,omitempty"`
	T        []byte `json:"t,omitempty"`
	R        []byte `json:"r,omitempty"`
	Answer   []byte `json:"answer,omitempty"`
	Confirm  []byte `json:"confirm,omitempty"`
	Done     []byte `json:"done,omitempty"`
}

// State is everything in state.json.
type State struct {
	Version    int              `json:"version"`
	Bsid       string           `json:"bsid"`
	Bkid       string           `json:"bkid"`
	Devices    []*Device        `json:"devices"`
	Items      map[string]*Item `json:"items"`
	Tombstones map[string]int64 `json:"tombstones"`
	Ops        map[string]*Op   `json:"ops"`
	Pairing    *Pairing         `json:"pairing,omitempty"`
	// RevokedIDs keeps every device id ever revoked, for ever (spec 9.4): the
	// detailed device list is pruned, the id set is not.
	RevokedIDs map[string]int64 `json:"revoked_ids"`
}

func newState() *State {
	return &State{Version: 1, Items: map[string]*Item{}, Tombstones: map[string]int64{},
		Ops: map[string]*Op{}, RevokedIDs: map[string]int64{}}
}

// activeDevice returns the device with id dev when it is not revoked.
func (s *State) activeDevice(dev string) *Device {
	for _, d := range s.Devices {
		if d.Dev == dev && !d.Revoked {
			return d
		}
	}
	return nil
}

func (s *State) activeCount() int {
	n := 0
	for _, d := range s.Devices {
		if !d.Revoked {
			n++
		}
	}
	return n
}

func (s *State) liveCount(kind string) int {
	n := 0
	for _, it := range s.Items {
		if it.Kind == kind && it.Status != StatusTombstoned {
			n++
		}
	}
	return n
}

// sortedItems returns live items of a kind ordered by id.
func (s *State) sortedItems(kind string) []*Item {
	var out []*Item
	for _, it := range s.Items {
		if it.Kind == kind && it.Status == StatusLive {
			out = append(out, it)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// ---- persistence ----

// errStateUncertain means the new file was renamed into place but the
// directory fsync failed: the outcome is unknown, so the caller must stop
// (spec 19 R2).
var errStateUncertain = errors.New("state write outcome uncertain")

// writeFileDurable writes data via temp file, fsync, rename, fsync dir. A
// failure before the rename leaves the old file intact (plain error); a
// failure after it returns errStateUncertain.
func writeFileDurable(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	renamed := false
	defer func() {
		if !renamed {
			os.Remove(name)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	renamed = true
	if err := stateDirSync(dir); err != nil {
		return errStateUncertain
	}
	return nil
}

// stateDirSync is the dir fsync of writeFileDurable; tests replace it.
var stateDirSync = fsyncDirPath

// mkdirAllDurable creates dir and its missing parents (0700) and fsyncs the
// parent of every directory on the way (code r2 #13).
func mkdirAllDurable(dir string) error {
	dir = filepath.Clean(dir)
	parent := filepath.Dir(dir)
	if parent != dir && parent != "/" {
		// Always walk the whole chain: an earlier attempt may have created a
		// parent and failed its barrier (code r3 #13).
		if err := mkdirAllDurable(parent); err != nil {
			return err
		}
	}
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	return fsyncDirPath(parent)
}

func fsyncDirPath(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func loadState(path string) (*State, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return newState(), nil
	}
	if err != nil {
		return nil, err
	}
	s := newState()
	if err := json.Unmarshal(b, s); err != nil {
		return nil, err
	}
	if s.Items == nil {
		s.Items = map[string]*Item{}
	}
	if s.Tombstones == nil {
		s.Tombstones = map[string]int64{}
	}
	if s.Ops == nil {
		s.Ops = map[string]*Op{}
	}
	if s.RevokedIDs == nil {
		s.RevokedIDs = map[string]int64{}
	}
	return s, nil
}

// ---- box keys ----

// BoxKeys is the box signing key and intake key.
type BoxKeys struct {
	Sign   *vcrypto.SigningKey
	Intake *vcrypto.AgreementKey
	Bsid   string
	Bkid   string
}

// Fingerprint is H("agentd-vault-v1 box-keys\n" || bsig_pub || bint_pub).
func (k *BoxKeys) Fingerprint() []byte {
	return vcrypto.Hash(vcrypto.Cat([]byte(vcrypto.LabelBoxKeys), k.Sign.Public(), k.Intake.Public()))
}

func newBoxKeys() *BoxKeys {
	k := &BoxKeys{Sign: vcrypto.NewSigningKey(), Intake: vcrypto.NewAgreementKey()}
	k.Bsid = vcrypto.KeyID(k.Sign.Public())
	k.Bkid = vcrypto.KeyID(k.Intake.Public())
	return k
}

// Key files are named by key id (spec 9.1), so a reset never overwrites the
// keys that state.json still points to.
func keyPaths(dir, bsid, bkid string) (string, string) {
	return filepath.Join(dir, "keys", bsid+".sign"), filepath.Join(dir, "keys", bkid+".intake")
}

func loadBoxKeys(dir, bsid, bkid string) (*BoxKeys, error) {
	sp, ip := keyPaths(dir, bsid, bkid)
	sb, err := os.ReadFile(sp)
	if err != nil {
		return nil, err
	}
	ib, err := os.ReadFile(ip)
	if err != nil {
		return nil, err
	}
	sk, err := vcrypto.SigningKeyFromScalar(sb)
	if err != nil {
		return nil, err
	}
	ik, err := vcrypto.AgreementKeyFromScalar(ib)
	if err != nil {
		return nil, err
	}
	k := &BoxKeys{Sign: sk, Intake: ik, Bsid: vcrypto.KeyID(sk.Public()), Bkid: vcrypto.KeyID(ik.Public())}
	if k.Bsid != bsid || k.Bkid != bkid {
		return nil, errors.New("key file does not match its id")
	}
	return k, nil
}

// saveBoxKeys writes both key files durably (temp, fsync, rename, fsync dir)
// before any state refers to them.
func saveBoxKeys(dir string, k *BoxKeys) error {
	if err := os.MkdirAll(filepath.Join(dir, "keys"), 0o700); err != nil {
		return err
	}
	sp, ip := keyPaths(dir, k.Bsid, k.Bkid)
	if err := writeFileDurable(sp, k.Sign.Scalar()); err != nil {
		return err
	}
	return writeFileDurable(ip, k.Intake.Scalar())
}

// removeOtherKeys deletes key files that state no longer points to and
// fsyncs the keys dir.
func removeOtherKeys(dir, bsid, bkid string) error {
	kd := filepath.Join(dir, "keys")
	ents, err := os.ReadDir(kd)
	if err != nil {
		return err
	}
	keep := map[string]bool{bsid + ".sign": true, bkid + ".intake": true}
	for _, e := range ents {
		if !keep[e.Name()] {
			if err := os.Remove(filepath.Join(kd, e.Name())); err != nil {
				return err
			}
		}
	}
	// Always: an earlier unlink whose dir fsync failed needs its barrier too.
	return fsyncDirPath(kd)
}

// cleanStateTemps removes leftover temp files of writeFileDurable (they can
// hold journal ciphertext) and fsyncs the dirs. Errors fail the start.
func cleanStateTemps(dir string) error {
	for _, d := range []string{dir, filepath.Join(dir, "keys")} {
		ents, err := os.ReadDir(d)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		for _, e := range ents {
			if len(e.Name()) > 5 && e.Name()[:5] == ".tmp-" {
				if err := os.Remove(filepath.Join(d, e.Name())); err != nil {
					return err
				}
			}
		}
		if err := fsyncDirPath(d); err != nil {
			return err
		}
	}
	return nil
}

// pruneDevices keeps every active device and the last 20 revoked ones.
func (s *State) pruneDevices() {
	var active, revoked []*Device
	for _, d := range s.Devices {
		if d.Revoked {
			revoked = append(revoked, d)
		} else {
			active = append(active, d)
		}
	}
	if len(revoked) > 20 {
		revoked = revoked[len(revoked)-20:]
	}
	s.Devices = append(active, revoked...)
}

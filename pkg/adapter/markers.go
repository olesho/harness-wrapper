package adapter

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/olesho/harness-wrapper/pkg/contract"
)

// Marker is the evidence that an input was handed to its harness: Send writes
// and syncs it before the harness gets the input, so an input without one
// never ran.
type Marker struct {
	InputID string `json:"input_id"`
	// Native is the input's id in the harness's own terms: the id its
	// record carries, which Readers map back to InputID.
	Native    string    `json:"native"`
	SessionID string    `json:"session_id"`
	At        time.Time `json:"at"`
}

// ErrMarked reports a second input under an input id that already has a
// marker: input ids are unique per agent for all time.
var ErrMarked = errors.New("adapter: the input id already has a submission marker")

// markersDir is where an agent's markers live, under its scratch root.
const markersDir = "markers"

// storeFile is created with the marker store. A store without it is not the
// one the markers were written to, so it proves no absence.
const storeFile = ".store"

// Markers is an agent's submission markers, in its scratch root: one file per
// input, written once. Absence of a marker proves an input never reached its
// harness only while the store is intact: Lookup reports an error, never
// absence, when it cannot tell.
type Markers struct {
	dir string

	mu       sync.Mutex
	loaded   bool
	byNative map[string]Marker
	// scannedMod is the store directory's mtime as of the last scan, and
	// scannedAt when that scan began; scans counts them, for tests.
	scannedMod, scannedAt time.Time
	scans                 int
}

// markerScanSlack is how close to a scan a directory mtime must be for the
// scan to be not trusted to have seen every entry: a filesystem with coarse
// timestamps can take a second entry within the same tick as the scan without
// changing the mtime again.
var markerScanSlack = 2 * time.Second

// OpenMarkers opens the marker store under scratch, creating it the first
// time.
func OpenMarkers(scratch string) (*Markers, error) {
	dir := filepath.Join(scratch, markersDir)
	switch _, err := os.Lstat(dir); {
	case errors.Is(err, fs.ErrNotExist):
		if err := createStore(scratch, dir); err != nil {
			return nil, fmt.Errorf("adapter: marker store: %w", err)
		}
	case err != nil:
		return nil, fmt.Errorf("adapter: marker store: %w", err)
	}
	return &Markers{dir: dir, byNative: map[string]Marker{}}, nil
}

// createStore creates the store whole: a directory holding storeFile, made
// beside it and renamed into place, so another Session's Host opening at the
// same moment never finds it without its sentinel. When another Host's store
// is there first, this one's is dropped: a store is never renamed over, since
// it holds its sentinel and rename replaces no directory with entries.
func createStore(scratch, dir string) error {
	tmp, err := os.MkdirTemp(scratch, markersDir+".tmp-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmp) }() // nothing left once renamed
	if err := writeSynced(filepath.Join(tmp, storeFile), []byte("harness-wrapper submission markers\n")); err != nil {
		return err
	}
	if err := syncDir(tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, dir); err != nil {
		if _, serr := os.Lstat(dir); serr == nil {
			return nil
		}
		return err
	}
	return syncDir(scratch)
}

// fileName is the marker file of inputID: a digest, so that no id — ".." is
// one — names a path of its own.
func fileName(inputID string) string {
	sum := sha256.Sum256([]byte(inputID))
	return hex.EncodeToString(sum[:]) + ".json"
}

// intact reports whether the store is the one markers were written to.
func (m *Markers) intact() error {
	if _, err := os.Lstat(filepath.Join(m.dir, storeFile)); err != nil {
		return fmt.Errorf("adapter: the marker store is not intact: %w", err)
	}
	return nil
}

// Write records mk durably: when it returns nil, the marker survives a crash.
// A marker already there for the input id is ErrMarked, and changes nothing.
//
// The claim is exclusive across processes and across Markers instances on
// one store (each Session opens its own, so m.mu alone guards nothing between
// them): the synced temp file is published with link(2), which fails with
// EEXIST when the name is taken, never with rename(2), which would replace a
// marker another writer published between the check and the rename.
func (m *Markers) Write(mk Marker) error {
	if !contract.ValidID(mk.InputID) || mk.Native == "" {
		return fmt.Errorf("adapter: marker for %q (native %q) is malformed", mk.InputID, mk.Native)
	}
	data, err := json.Marshal(mk)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.intact(); err != nil {
		return err
	}
	final := filepath.Join(m.dir, fileName(mk.InputID))
	if _, err := os.Lstat(final); err == nil {
		return ErrMarked
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("adapter: marker: %w", err)
	}
	if err := publishExclusive(final, data); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return ErrMarked
		}
		return fmt.Errorf("adapter: marker: %w", err)
	}
	if err := syncDir(m.dir); err != nil {
		return fmt.Errorf("adapter: marker: %w", err)
	}
	if m.loaded {
		m.byNative[mk.Native] = mk
	}
	return nil
}

// Withdraw removes the marker of an input its harness never got. Send withdraws
// it when the transport refused the input before anything reached the harness,
// so the input id is free to be sent again and Recover finds it never
// submitted. Withdrawing a marker that is not there changes nothing.
func (m *Markers) Withdraw(inputID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.intact(); err != nil {
		return err
	}
	path := filepath.Join(m.dir, fileName(inputID))
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("adapter: marker: %w", err)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("adapter: marker: %w", err)
	}
	if err := syncDir(m.dir); err != nil {
		return fmt.Errorf("adapter: marker: %w", err)
	}
	var mk Marker
	if json.Unmarshal(data, &mk) == nil && mk.Native != "" {
		delete(m.byNative, mk.Native)
	}
	return nil
}

// Lookup reads the marker of inputID. found is false only when the intact
// store has none; an unreadable or corrupt store or marker is an error.
func (m *Markers) Lookup(inputID string) (mk Marker, found bool, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.intact(); err != nil {
		return Marker{}, false, err
	}
	data, err := os.ReadFile(filepath.Join(m.dir, fileName(inputID)))
	if errors.Is(err, fs.ErrNotExist) {
		return Marker{}, false, nil
	}
	if err != nil {
		return Marker{}, false, fmt.Errorf("adapter: marker: %w", err)
	}
	if err := json.Unmarshal(data, &mk); err != nil || mk.InputID != inputID || mk.Native == "" {
		return Marker{}, false, fmt.Errorf("adapter: the marker of %q is corrupt", inputID)
	}
	return mk, true, nil
}

// ByNative finds the marker of the input whose native id is native. A marker
// written by this process is found at once; one written by another is found
// once the store is read again, which a miss does when the store directory
// changed since it was last read. Most lookups miss — a reader asks about
// every user entry, tool results included — so a miss on an unchanged store
// costs one stat, not a read of every marker.
func (m *Markers) ByNative(native string) (Marker, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if mk, ok := m.byNative[native]; ok {
		return mk, true
	}
	if !m.staleLocked() {
		return Marker{}, false
	}
	m.loadLocked()
	mk, ok := m.byNative[native]
	return mk, ok
}

// staleLocked reports whether the store may hold markers the index has not
// read: it was never read, its directory cannot be stat'ed, its mtime moved
// since the last scan, or that mtime was too close to the scan to rule out a
// change in the same timestamp tick.
func (m *Markers) staleLocked() bool {
	if !m.loaded {
		return true
	}
	fi, err := os.Stat(m.dir)
	if err != nil {
		return true
	}
	return !fi.ModTime().Equal(m.scannedMod) || m.scannedAt.Sub(m.scannedMod) < markerScanSlack
}

// loadLocked reads every marker into the native index. Markers it cannot read
// are skipped: they map no native id, which leaves their input's turn
// unattributed, never misattributed.
func (m *Markers) loadLocked() {
	m.loaded = true
	m.scans++
	// The mtime is taken before the listing, so an entry added during the
	// scan moves it past what is recorded and the next miss reads again.
	m.scannedAt = time.Now()
	m.scannedMod = time.Time{}
	if fi, err := os.Stat(m.dir); err == nil {
		m.scannedMod = fi.ModTime()
	}
	entries, err := os.ReadDir(m.dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if !e.Type().IsRegular() || !strings.HasSuffix(name, ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(m.dir, name))
		if err != nil {
			continue
		}
		var mk Marker
		if json.Unmarshal(data, &mk) != nil || mk.Native == "" || fileName(mk.InputID) != name {
			continue
		}
		m.byNative[mk.Native] = mk
	}
}

// publishExclusive makes final hold data, synced, unless something is already
// at final, which it reports as an error wrapping fs.ErrExist. data is written
// and synced to a temp file first and linked to final, so a reader never sees
// a partial marker; the temp is removed either way. The caller syncs the
// directory. A filesystem without hard links falls back to creating final
// itself with O_EXCL, exclusive too, though a reader may then see it partly
// written.
func publishExclusive(final string, data []byte) error {
	tmp := final + ".tmp-" + randomHex(8)
	if err := writeSynced(tmp, data); err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp) }()
	err := os.Link(tmp, final)
	if errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.EOPNOTSUPP) || errors.Is(err, syscall.EPERM) {
		return writeSynced(final, data)
	}
	return err
}

// writeSynced creates path holding data, synced. O_EXCL: nothing already at
// path is written through.
func writeSynced(path string, data []byte) (err error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // a path inside the agent's scratch root
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = os.Remove(path)
		}
	}()
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// syncDir makes the entries created, renamed or removed in dir durable. A
// filesystem that cannot sync a directory has nothing more to make durable.
func syncDir(dir string) error {
	d, err := os.Open(dir) //nolint:gosec // a directory of the agent's scratch root
	if err != nil {
		return err
	}
	err = d.Sync()
	if errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.EOPNOTSUPP) {
		err = nil
	}
	if cerr := d.Close(); err == nil {
		err = cerr
	}
	return err
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

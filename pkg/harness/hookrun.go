package harness

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/olesho/harness-wrapper/pkg/transcript"
)

// spoolSeq disambiguates spool filenames for concurrent writers within ONE
// process (the wall clock can repeat a nanosecond under contention); pid
// disambiguates across the separate hook subprocesses.
var spoolSeq atomic.Uint64

// HW_* env var names. The orchestrator SETS them in the harness launch env, the
// harness propagates them to hook subprocesses, and HandleHookEvent READS them.
// They are the authority for the hook ENVIRONMENT (never the subprocess cwd).
const (
	EnvSpool     = "HW_EVENT_SPOOL"        // spool dir; absent ⇒ handler inert
	EnvHookCwd   = "HW_HOOK_CWD"           // harness working dir (worktree)
	EnvHome      = "HW_HOME"               // user home
	EnvConfigDir = "HW_HARNESS_CONFIG_DIR" //nolint:gosec // env var NAME, not a credential
	// EnvHarnessSessionID is the native session id a RESUME launch is resuming.
	// Set only on resume (and only when non-empty); it arms the resume session
	// guard in HandleHookEvent that drops a stale/leftover hook fired for a
	// DIFFERENT session lingering in the same per-run spool. Absent (fresh
	// starts, codex, any non-resume launch) ⇒ the guard is disarmed.
	EnvHarnessSessionID = "HW_HARNESS_SESSION_ID" //nolint:gosec // env var NAME, not a credential
)

// HandleHookEvent is the entrypoint the thin `loom hooks <harness> <event>`
// command delegates to. For capture events it parses the fired hook's stdin
// payload into events and writes them to the spool; for the yield-guard control
// event it returns a HookOutcome telling the caller whether to BLOCK the tool.
//
// It is INERT (zero outcome, writes nothing) when HW_EVENT_SPOOL is absent — so
// a leftover hook entry can never perturb a non-wrapper run (review #5; this is
// the runtime counterpart to the rendered shell guard). The subprocess does NOT
// call Resolve: it obtains the harness's STATIC HookProvider and trusts that the
// main run's resolution already decided to install hooks.
func HandleHookEvent(harnessName, event string, env []string, stdin []byte) (HookOutcome, error) {
	spool := EnvLookup(env, EnvSpool)
	if spool == "" {
		return HookOutcome{}, nil // inert outside a wrapper run
	}
	p, ok := For(harnessName)
	if !ok {
		return HookOutcome{}, fmt.Errorf("harness: no profile registered for %q", harnessName)
	}
	shp, ok := p.(StaticHookProfile)
	if !ok {
		return HookOutcome{}, fmt.Errorf("harness: %q has no hook provider", harnessName)
	}
	hp := shp.StaticHookProvider()

	// The yield-guard is a control hook, not a capture hook: it decides whether
	// to block the tool, and never touches the spool.
	if spec := hp.HookSpec(); spec.Yield != nil && event == spec.Yield.Arg {
		return checkYield(EnvLookup(env, EnvYieldFile)), nil
	}

	ctx := HookContext{
		Cwd:              EnvLookup(env, EnvHookCwd),
		Home:             EnvLookup(env, EnvHome),
		ConfigDir:        EnvLookup(env, EnvConfigDir),
		SpoolDir:         spool,
		HarnessSessionID: EnvLookup(env, EnvHarnessSessionID),
	}
	events, err := hp.ParseHookPayload(ctx, event, stdin)
	if err != nil {
		return HookOutcome{}, fmt.Errorf("harness: parse hook %s/%s: %w", harnessName, event, err)
	}
	events = filterResumeSession(ctx.HarnessSessionID, events)
	if len(events) == 0 {
		return HookOutcome{}, nil
	}
	return HookOutcome{}, writeSpool(spool, event, events)
}

// writeSpool writes one batch of parsed events to the spool as a single file,
// crash-safely: marshal → write a unique `.tmp` and fsync it → rename(2) into
// place → fsync the directory. The rename is atomic and lock-free, so
// concurrent hook subprocesses (e.g. overlapping PostToolUse[Task] + Stop)
// never produce a partial or torn file, and a reader (which reads only
// completed `.json` files) never sees half a record (review #6). The fsyncs
// make a nil return mean durable: without the first, a power failure can leave
// the renamed file empty; without the second, it can undo the rename and lose
// a batch the hook already reported written.
func writeSpool(spoolDir, event string, events []transcript.ParsedEvent) error {
	// DURABLE form: persists Source/NativeID/SchemaVersion (Event's public JSON
	// omits them), which the authority filter + dedup need after the round-trip.
	data, err := transcript.MarshalParsedEvents(events)
	if err != nil {
		return fmt.Errorf("harness: marshal spool events: %w", err)
	}
	// Unique per (event, time, pid, in-process seq): pids differ across the
	// separate hook subprocesses, and the atomic seq guarantees uniqueness for
	// concurrent writers within one process even at the same nanosecond.
	// Ordering is reconstructed from event content, not the filename.
	base := fmt.Sprintf("%s-%d-%d-%d.json", event, time.Now().UnixNano(), os.Getpid(), spoolSeq.Add(1))
	final := filepath.Join(spoolDir, base)
	tmp := final + ".tmp"
	if err := writeSynced(tmp, data); err != nil {
		return fmt.Errorf("harness: write spool temp: %w", err)
	}
	if err := spoolCrash("write:synced"); err != nil {
		return err
	}
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("harness: commit spool file: %w", err)
	}
	if err := spoolCrash("write:renamed"); err != nil {
		return err
	}
	if err := syncDir(spoolDir); err != nil {
		return fmt.Errorf("harness: sync spool dir: %w", err)
	}
	return spoolCrash("write:committed")
}

// writeSynced creates path and fsyncs data to it. O_EXCL means a file or
// symlink already at path is never written through; the file is removed again
// if any step fails.
func writeSynced(path string, data []byte) (err error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // path is inside the wrapper-owned spool dir
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

// syncDir fsyncs the directory at path, making the entries created, renamed or
// removed in it durable.
func syncDir(path string) error {
	d, err := os.Open(path) //nolint:gosec // the wrapper-owned spool dir
	if err != nil {
		return err
	}
	return syncDirFile(d)
}

// syncDirFile fsyncs and closes an open directory. A filesystem that cannot
// fsync a directory at all (EINVAL, ENOTSUP) has nothing more to make durable,
// so that is not an error.
func syncDirFile(d *os.File) error {
	err := d.Sync()
	if errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.EOPNOTSUPP) {
		err = nil
	}
	if cerr := d.Close(); err == nil {
		err = cerr
	}
	return err
}

// spoolCrash marks each durability boundary of writeSpool and AckSpool. It
// never fails in production. The spool tests substitute one that fails at a
// chosen point to simulate a crash there: the caller returns at once, leaving
// the disk exactly as that crash would.
var spoolCrash = func(string) error { return nil }

// DrainSpool reads every COMPLETED spool file (`.json`, never the in-flight
// `.tmp`), returning all parsed events and removing each file it successfully
// consumed so a later drain does not re-emit them. Run calls it after the
// harness exits (the grace-window drain), then removes the per-run spool; the
// consumer additionally dedups by Event.ID(), so a file left behind by a delete
// failure is absorbed rather than duplicated.
//
// DrainSpool is destructive and not transactional: it deletes each file as
// soon as it has parsed it, before the caller has done anything with the
// events, so a crash in between loses them. That suits a consumer that lives
// and dies with the run. One that must not lose events — agentd, whose
// supervisor outlives its harness — reads with ReadSpool and acknowledges
// with AckSpool after its own durable commit, and never calls DrainSpool.
//
// A missing spool dir is not an error (no hooks fired). A single unreadable /
// unparseable file is skipped (left in place) and collected into err, but does
// not abort the drain of the rest.
func DrainSpool(spoolDir string) ([]transcript.ParsedEvent, error) {
	entries, err := os.ReadDir(spoolDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("harness: read spool dir: %w", err)
	}
	var (
		out  []transcript.ParsedEvent
		errs []string
	)
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") {
			continue // skip dirs and in-flight *.json.tmp files
		}
		path := filepath.Join(spoolDir, name)
		data, rerr := os.ReadFile(path) //nolint:gosec // path is inside the wrapper-owned spool dir
		if rerr != nil {
			errs = append(errs, rerr.Error())
			continue
		}
		evs, uerr := transcript.UnmarshalParsedEvents(data)
		if uerr != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", name, uerr))
			continue // leave malformed file in place for inspection
		}
		out = append(out, evs...)
		_ = os.Remove(path) // consumed; dedup-by-ID covers a failed remove
	}
	if len(errs) > 0 {
		return out, fmt.Errorf("harness: spool drain skipped %d file(s): %s", len(errs), strings.Join(errs, "; "))
	}
	return out, nil
}

// MaxSpoolFileBytes bounds one spool file. ReadSpool sets a larger file aside
// unread, and AckSpool refuses a receipt that claims more. A Stop hook spools
// the whole parent conversation, so a file grows with its conversation; the
// bound is far above any real one and exists so that a planted file cannot
// exhaust the reader's memory.
const MaxSpoolFileBytes = 64 << 20

// SpoolQuarantineDir is the subdirectory of a spool where ReadSpool sets aside
// the files it cannot read as events, for inspection.
const SpoolQuarantineDir = "quarantine"

// MaxSpoolQuarantine bounds the files SpoolQuarantineDir keeps. Once it holds
// this many, ReadSpool deletes further unreadable files instead, and says so.
const MaxSpoolQuarantine = 32

// spoolReadBytes and spoolReadFiles bound one ReadSpool call — the bytes it
// reads and the files it handles — so that a flooded spool cannot exhaust the
// reader's memory or stall it; the rest waits for the next call. They are
// variables so tests can shrink them.
var (
	spoolReadBytes int64 = MaxSpoolFileBytes
	spoolReadFiles       = 1024
)

// ErrSpoolReceipt is the error AckSpool wraps when it refuses a receipt: its
// name is not a plain spool file name, or its file is no longer the one
// ReadSpool read — the contents changed, or something else took its place. A
// refused receipt deletes nothing.
var ErrSpoolReceipt = errors.New("harness: spool receipt refused")

// SpoolReceipt identifies one spool file exactly as ReadSpool read it. A
// consumer hands it back to AckSpool once it has durably committed the file's
// events. It is comparable, so the consumer can record it in its own journal
// and recognise the file when a crash makes ReadSpool return it again.
type SpoolReceipt struct {
	// Name is the file's name inside the spool directory, never a path.
	Name string
	// Size is the number of bytes read.
	Size int64
	// Digest is "sha256:" followed by the lowercase hex SHA-256 of those bytes.
	Digest string
}

// SpoolBatch is the events one spool file holds, with the receipt that
// acknowledges it.
type SpoolBatch struct {
	Receipt SpoolReceipt
	Events  []transcript.ParsedEvent
}

// SpoolQuarantine reports a file ReadSpool took out of the spool without
// returning its events, because it could not trust or parse it.
type SpoolQuarantine struct {
	// Name is the file's name in the spool directory.
	Name string
	// Reason says what was wrong with the file.
	Reason string
	// Deleted reports that the file was deleted rather than kept in
	// SpoolQuarantineDir, because the quarantine was full or unusable.
	Deleted bool
}

// SpoolContents is what one ReadSpool call found.
type SpoolContents struct {
	// Batches holds the events of each spool file read, in file-name order.
	Batches []SpoolBatch
	// Quarantined reports each file taken out of the spool unread.
	Quarantined []SpoolQuarantine
	// More reports that the call stopped at its bound with files left to
	// read: acknowledge these batches, then call ReadSpool again.
	More bool
}

// ReadSpool reads the completed spool files (`.json`, never an in-flight
// `.tmp`) without consuming them. Each batch carries a receipt, and its file
// stays in the spool until AckSpool is handed that receipt. A consumer commits
// a batch's events durably first and acknowledges it second, so a crash at any
// point loses nothing: ReadSpool returns every file not yet acknowledged again,
// under the same receipt, for the consumer to recognise as committed (or to
// dedup by Event.ID()).
//
// The spool is written by the hook subprocess, which may run as a less trusted
// user than the reader, so ReadSpool trusts nothing in it. It refuses a spool
// dir that is itself a symlink and confines every lookup to the directory. It
// reads only regular, singly linked files of at most MaxSpoolFileBytes, and it
// never follows a symlink or blocks on a FIFO. A file it cannot read as events
// — any other kind of file, an oversize or hard-linked one, an unsafe name, or
// contents that do not parse — is moved into SpoolQuarantineDir, or deleted
// once that holds MaxSpoolQuarantine files, and reported in Quarantined. It
// is never lost silently, and it cannot block the files behind it.
//
// One call handles a bounded amount of the spool and sets More when it leaves
// files for the next call. A spool has one consumer: ReadSpool takes no lock,
// so two concurrent readers would each return the same files. A missing spool
// dir is not an error (no hook fired). A file that cannot be read for any
// other reason stays where it is and is named in the error; the batches
// returned alongside an error are still valid.
func ReadSpool(spoolDir string) (SpoolContents, error) {
	var out SpoolContents
	root, err := openSpool(spoolDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return out, nil
		}
		return out, err
	}
	defer func() { _ = root.Close() }()
	dir, err := root.Open(".")
	if err != nil {
		return out, fmt.Errorf("harness: open spool dir: %w", err)
	}
	defer func() { _ = dir.Close() }()

	var (
		errs    []error
		read    int64
		handled int
		q       = quarantiner{root: root}
	)
list:
	for {
		entries, lerr := dir.ReadDir(256)
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".json") {
				continue // the quarantine, in-flight *.json.tmp files and anything else
			}
			if handled == spoolReadFiles {
				out.More = true
				break list
			}
			handled++
			fi, err := root.Lstat(name)
			if errors.Is(err, fs.ErrNotExist) {
				continue // acknowledged or removed since the listing
			}
			if err != nil {
				errs = append(errs, fmt.Errorf("harness: spool file %q: %w", name, err))
				continue
			}
			if reason := untrustedSpoolFile(name, fi); reason != "" {
				q.take(&out, &errs, name, reason)
				continue
			}
			if read > 0 && read+fi.Size() > spoolReadBytes {
				out.More = true
				break list
			}
			batch, n, reason, err := readSpoolFile(root, name, fi)
			read += n
			switch {
			case errors.Is(err, fs.ErrNotExist):
				// acknowledged or removed since the Lstat
			case err != nil:
				errs = append(errs, fmt.Errorf("harness: spool file %q: %w", name, err))
			case reason != "":
				q.take(&out, &errs, name, reason)
			default:
				out.Batches = append(out.Batches, batch)
			}
		}
		if lerr != nil {
			if !errors.Is(lerr, io.EOF) {
				errs = append(errs, fmt.Errorf("harness: list spool dir: %w", lerr))
			}
			break
		}
	}
	slices.SortFunc(out.Batches, func(a, b SpoolBatch) int { return strings.Compare(a.Receipt.Name, b.Receipt.Name) })
	slices.SortFunc(out.Quarantined, func(a, b SpoolQuarantine) int { return strings.Compare(a.Name, b.Name) })
	return out, errors.Join(errs...)
}

// AckSpool deletes the spool files the receipts name, once the consumer has
// durably committed their events, and fsyncs the directory so that the
// deletions survive a crash. It deletes a file only while it is still exactly
// the one ReadSpool read. A receipt whose name is not a plain spool file name,
// or whose file changed or was replaced since, is refused — wrapped in
// ErrSpoolReceipt — and deletes nothing, so contents nobody committed are
// never lost: ReadSpool returns the changed file again under a new receipt.
//
// A receipt whose file is already gone counts as acknowledged, so repeating
// an acknowledgement, say after a crash, is safe; the directory is fsynced
// even then, which makes an earlier unsynced deletion durable. Until AckSpool
// returns nil, a crash can leave any of its files in place to be read again,
// and the consumer recognises them by receipt. The error joins every refusal
// and failure; the other receipts are still acknowledged.
func AckSpool(spoolDir string, receipts ...SpoolReceipt) error {
	if len(receipts) == 0 {
		return nil
	}
	root, err := openSpool(spoolDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil // no spool, so nothing is left to delete
		}
		return err
	}
	defer func() { _ = root.Close() }()
	var errs []error
	for _, r := range receipts {
		st, gone, err := verifySpoolReceipt(root, r)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if gone {
			continue
		}
		if err := spoolCrash("ack:verified"); err != nil {
			return err
		}
		if err := removeVerified(root, r.Name, st); err != nil {
			errs = append(errs, err)
			continue
		}
		if err := spoolCrash("ack:removed"); err != nil {
			return err
		}
	}
	d, err := root.Open(".")
	if err == nil {
		err = syncDirFile(d)
	}
	if err != nil {
		return errors.Join(append(errs, fmt.Errorf("harness: sync spool dir: %w", err))...)
	}
	if err := spoolCrash("ack:committed"); err != nil {
		return err
	}
	return errors.Join(errs...)
}

// openSpool opens the spool directory as an os.Root, which confines every
// later lookup to it. A spool dir that is itself a symlink is refused, and so
// is one swapped for a symlink while it is opened: a writer that can replace
// the directory must not redirect the reader elsewhere.
func openSpool(spoolDir string) (*os.Root, error) {
	fi, err := os.Lstat(spoolDir)
	if err != nil {
		return nil, fmt.Errorf("harness: spool dir: %w", err)
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("harness: spool dir %s is %s, not a directory", spoolDir, fileKind(fi.Mode()))
	}
	root, err := os.OpenRoot(spoolDir)
	if err != nil {
		return nil, fmt.Errorf("harness: open spool dir: %w", err)
	}
	if ri, err := root.Stat("."); err != nil || !os.SameFile(fi, ri) {
		_ = root.Close()
		return nil, fmt.Errorf("harness: spool dir %s was replaced while it was opened", spoolDir)
	}
	return root, nil
}

// openSpoolFile opens a spool entry for reading. O_NONBLOCK keeps a FIFO
// swapped in after the Lstat from blocking the open, and O_NOCTTY keeps a
// terminal from becoming the reader's controlling one.
func openSpoolFile(root *os.Root, name string) (*os.File, error) {
	return root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOCTTY, 0)
}

// validSpoolName reports whether name is a plain spool file name: a `.json`
// name with no path separator, so it can only ever name a file directly inside
// the spool directory.
func validSpoolName(name string) bool {
	return strings.HasSuffix(name, ".json") && len(name) <= 255 && !strings.ContainsAny(name, "/\\\x00")
}

// untrustedSpoolFile returns why ReadSpool must not read the spool entry name,
// judged from its Lstat, or "" when it may.
func untrustedSpoolFile(name string, fi fs.FileInfo) string {
	if !validSpoolName(name) {
		return "not a plain spool file name"
	}
	if !fi.Mode().IsRegular() {
		return fileKind(fi.Mode()) + ", not a regular file"
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && st.Nlink > 1 {
		return fmt.Sprintf("hard-linked (%d links); the writer never links a spool file", st.Nlink)
	}
	if fi.Size() > MaxSpoolFileBytes {
		return fmt.Sprintf("%d bytes, over the %d-byte limit", fi.Size(), MaxSpoolFileBytes)
	}
	return ""
}

// readSpoolFile reads and parses the spool file name, which fi (its Lstat)
// found to be a regular file. It returns the bytes it read and either the
// batch, or a reason to quarantine the file, or an error that leaves the file
// where it is.
func readSpoolFile(root *os.Root, name string, fi fs.FileInfo) (SpoolBatch, int64, string, error) {
	f, err := openSpoolFile(root, name)
	if err != nil {
		return SpoolBatch{}, 0, "", err
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return SpoolBatch{}, 0, "", err
	}
	if !os.SameFile(fi, st) {
		return SpoolBatch{}, 0, "", errors.New("replaced while it was being read")
	}
	// Sized from the Stat, so that a file up to the bound is read in one
	// allocation, but never past the bound: the file may have grown since the
	// Lstat, and the limit stops one that grows while it is read.
	buf := bytes.NewBuffer(make([]byte, 0, min(st.Size(), MaxSpoolFileBytes)+bytes.MinRead))
	_, err = buf.ReadFrom(io.LimitReader(f, MaxSpoolFileBytes+1))
	data := buf.Bytes()
	n := int64(len(data))
	if err != nil {
		return SpoolBatch{}, n, "", err
	}
	if n > MaxSpoolFileBytes {
		return SpoolBatch{}, n, fmt.Sprintf("grew past the %d-byte limit while it was read", MaxSpoolFileBytes), nil
	}
	events, err := transcript.UnmarshalParsedEvents(data)
	if err != nil {
		return SpoolBatch{}, n, fmt.Sprintf("malformed: %v", err), nil
	}
	sum := sha256.Sum256(data)
	receipt := SpoolReceipt{Name: name, Size: n, Digest: "sha256:" + hex.EncodeToString(sum[:])}
	return SpoolBatch{Receipt: receipt, Events: events}, n, "", nil
}

// verifySpoolReceipt checks that the spool file r names is still exactly the
// one ReadSpool read, and returns its Stat for removeVerified. gone reports
// that the file no longer exists, so the receipt is already acknowledged.
func verifySpoolReceipt(root *os.Root, r SpoolReceipt) (st fs.FileInfo, gone bool, err error) {
	if !validSpoolName(r.Name) {
		return nil, false, fmt.Errorf("%w: %q is not a plain spool file name", ErrSpoolReceipt, r.Name)
	}
	if r.Size < 0 || r.Size > MaxSpoolFileBytes || !validSpoolDigest(r.Digest) {
		return nil, false, fmt.Errorf("%w: %q: malformed receipt", ErrSpoolReceipt, r.Name)
	}
	fi, err := root.Lstat(r.Name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, true, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("harness: ack spool file %q: %w", r.Name, err)
	}
	if !fi.Mode().IsRegular() {
		return nil, false, fmt.Errorf("%w: %q is now %s", ErrSpoolReceipt, r.Name, fileKind(fi.Mode()))
	}
	if fi.Size() != r.Size {
		return nil, false, fmt.Errorf("%w: %q changed since it was read (%d bytes, was %d)", ErrSpoolReceipt, r.Name, fi.Size(), r.Size)
	}
	f, err := openSpoolFile(root, r.Name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, true, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("harness: ack spool file %q: %w", r.Name, err)
	}
	defer func() { _ = f.Close() }()
	if st, err = f.Stat(); err != nil {
		return nil, false, fmt.Errorf("harness: ack spool file %q: %w", r.Name, err)
	}
	if !os.SameFile(fi, st) {
		return nil, false, fmt.Errorf("%w: %q was replaced while it was verified", ErrSpoolReceipt, r.Name)
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, r.Size+1))
	if err != nil {
		return nil, false, fmt.Errorf("harness: ack spool file %q: %w", r.Name, err)
	}
	if n != r.Size || "sha256:"+hex.EncodeToString(h.Sum(nil)) != r.Digest {
		return nil, false, fmt.Errorf("%w: %q changed since it was read", ErrSpoolReceipt, r.Name)
	}
	return st, false, nil
}

// removeVerified deletes the spool file name if it is still the file verified
// as st. The check narrows, but cannot close, the window in which a writer
// could swap in another file; the spool's own writer never replaces a file.
func removeVerified(root *os.Root, name string, st fs.FileInfo) error {
	fi, err := root.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("harness: ack spool file %q: %w", name, err)
	}
	if !os.SameFile(st, fi) {
		return fmt.Errorf("%w: %q was replaced while it was verified", ErrSpoolReceipt, name)
	}
	if err := root.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("harness: ack spool file %q: %w", name, err)
	}
	return nil
}

// validSpoolDigest reports whether d has the form ReadSpool gives a digest.
func validSpoolDigest(d string) bool {
	hexPart, ok := strings.CutPrefix(d, "sha256:")
	if !ok || len(hexPart) != hex.EncodedLen(sha256.Size) {
		return false
	}
	for _, c := range hexPart {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// quarantiner takes the files one ReadSpool call cannot read out of the spool.
// It opens SpoolQuarantineDir on first use and counts what it holds.
type quarantiner struct {
	root   *os.Root
	opened bool
	held   int   // files SpoolQuarantineDir holds
	err    error // why SpoolQuarantineDir cannot be used, once known
}

// take moves the spool file name into the quarantine, or deletes it when the
// quarantine is full or unusable, and reports it in out. A file that has
// already gone was taken by another reader and is not reported. Neither the
// move nor the deletion is fsynced: a crash that undoes one only makes the
// next ReadSpool take the file again.
func (q *quarantiner) take(out *SpoolContents, errs *[]error, name, reason string) {
	if !q.opened {
		q.open()
	}
	switch {
	case q.err != nil:
		reason += "; deleted: the quarantine is unusable: " + q.err.Error()
	case q.held >= MaxSpoolQuarantine:
		reason += fmt.Sprintf("; deleted: the quarantine already holds %d files", MaxSpoolQuarantine)
	default:
		err := q.root.Rename(name, filepath.Join(SpoolQuarantineDir, name))
		if err == nil {
			q.held++
			out.Quarantined = append(out.Quarantined, SpoolQuarantine{Name: name, Reason: reason})
			return
		}
		if errors.Is(err, fs.ErrNotExist) {
			return
		}
		reason += "; deleted: it could not be quarantined: " + err.Error()
	}
	if err := q.root.Remove(name); err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			*errs = append(*errs, fmt.Errorf("harness: spool file %q (%s): %w", name, reason, err))
		}
		return
	}
	out.Quarantined = append(out.Quarantined, SpoolQuarantine{Name: name, Reason: reason, Deleted: true})
}

// open creates SpoolQuarantineDir if needed, checks that it is a real
// directory, and counts what it holds, up to the bound.
func (q *quarantiner) open() {
	q.opened = true
	if err := q.root.Mkdir(SpoolQuarantineDir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		q.err = err
		return
	}
	fi, err := q.root.Lstat(SpoolQuarantineDir)
	if err != nil {
		q.err = err
		return
	}
	if !fi.IsDir() {
		q.err = fmt.Errorf("%s is %s", SpoolQuarantineDir, fileKind(fi.Mode()))
		return
	}
	d, err := q.root.OpenFile(SpoolQuarantineDir, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NONBLOCK, 0)
	if err != nil {
		q.err = err
		return
	}
	defer func() { _ = d.Close() }()
	held, err := d.ReadDir(MaxSpoolQuarantine)
	if err != nil && !errors.Is(err, io.EOF) {
		q.err = err
		return
	}
	q.held = len(held)
}

// fileKind names the kind of file a mode describes, for diagnostics.
func fileKind(m fs.FileMode) string {
	switch m.Type() {
	case 0:
		return "a regular file"
	case fs.ModeDir:
		return "a directory"
	case fs.ModeSymlink:
		return "a symlink"
	case fs.ModeNamedPipe:
		return "a named pipe"
	case fs.ModeSocket:
		return "a socket"
	case fs.ModeDevice, fs.ModeDevice | fs.ModeCharDevice:
		return "a device"
	default:
		return "an irregular file"
	}
}

// filterResumeSession is the resume session guard (item 4 of the
// HARNESS-WRAPPER-52 plan). On a RESUME launch — expected != "" (from
// HW_HARNESS_SESSION_ID, set only on resume) — it drops a PARENT-conversation
// event whose session id mismatches: a stale/leftover hook fired for a
// DIFFERENT session that lingers in the SAME per-run spool. It bites only on
// resume within one worktree; the per-run temp spool (os.MkdirTemp "hw-spool-"
// + deferred RemoveAll) already isolates unrelated sessions, so this defends
// only the narrow residual where a resume reuses the spool across a session-id
// change. It mirrors TS's sessionMatches(expected, payload.session_id), which
// likewise bites only for a non-empty expected id.
//
// HandleHookEvent is the shared entrypoint for EVERY harness's hooks, so this
// also runs on the codex hook path — harmlessly: HW_HARNESS_SESSION_ID is set
// only on Claude resume launches, so for codex (and every fresh start) expected
// is empty and the guard is disarmed, returning events unchanged.
//
// SUBAGENT-SAFETY: subagent events carry their OWN native agentID in
// HarnessSessionID and the parent id in ParentSessionID (readSubagentTranscript),
// whereas parent events carry the fired session id in HarnessSessionID and an
// empty ParentSessionID (readParentTranscript / sessionMarker). A naive
// HarnessSessionID == expected filter would therefore DROP EVERY SUBAGENT EVENT
// on a resume (each subagent's id differs from the expected parent id), silently
// discarding legitimate nested runs. The only correct drop condition is a
// PARENT event (ParentSessionID == "") whose id mismatches; subagent events
// (ParentSessionID != "") are ALWAYS kept.
func filterResumeSession(expected string, events []transcript.ParsedEvent) []transcript.ParsedEvent {
	if expected == "" {
		return events // disarmed: fresh start / non-resume / codex
	}
	kept := events[:0]
	for _, pe := range events {
		if pe.ParentSessionID == "" && pe.HarnessSessionID != expected {
			continue // stale parent event for a different session — drop
		}
		kept = append(kept, pe) // matching parent, or any subagent event
	}
	return kept
}

// EnvLookup returns the value of key in an os.Environ()-style "K=V" slice, or ""
// if absent. The last occurrence wins (matching exec semantics). Exported so
// per-harness Profile packages — which cannot see this package's unexported
// helpers — read the launch env by exactly the same rule the hook subprocess
// does; see ConfigDirResolver.
func EnvLookup(env []string, key string) string {
	prefix := key + "="
	val := ""
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			val = kv[len(prefix):]
		}
	}
	return val
}

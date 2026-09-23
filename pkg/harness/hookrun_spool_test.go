package harness

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/pkg/transcript"
)

var errTestCrash = errors.New("simulated crash")

// spoolText writes one batch holding a single text event, as a hook would.
func spoolText(spool, text string) error {
	return writeSpool(spool, "stop", []transcript.ParsedEvent{{
		HarnessSessionID: "s",
		Event:            transcript.Event{Type: transcript.EventText, Text: text, Source: transcript.SourceFile},
	}})
}

// crashAt makes the spool crash at point until the returned function is
// called: the writeSpool or AckSpool passing that point stops there.
func crashAt(point string) (restart func()) {
	orig := spoolCrash
	spoolCrash = func(p string) error {
		if p == point {
			return errTestCrash
		}
		return nil
	}
	return func() { spoolCrash = orig }
}

// spoolJSON returns the completed spool files and their contents.
func spoolJSON(t *testing.T, spool string) map[string][]byte {
	t.Helper()
	entries, err := os.ReadDir(spool)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]byte{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(spool, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		out[e.Name()] = data
	}
	return out
}

// batchTexts returns the text of every event in batches, in order.
func batchTexts(batches []SpoolBatch) []string {
	var out []string
	for _, b := range batches {
		out = append(out, texts(b.Events)...)
	}
	return out
}

func receiptsOf(c SpoolContents) []SpoolReceipt {
	out := make([]SpoolReceipt, len(c.Batches))
	for i, b := range c.Batches {
		out[i] = b.Receipt
	}
	return out
}

// spoolJournal is the consumer's side of the spool contract, as agentd runs it:
// a poll reads the spool, commits each batch the journal does not already
// hold — recognised by its receipt — and then acknowledges every batch it read.
type spoolJournal struct {
	committed map[SpoolReceipt]bool
	texts     []string
}

// poll runs one read → commit → ack pass. crash names the boundary on the
// consumer's side where the pass stops, as a crash there would stop it: "read"
// (ReadSpool returned) or "commit" (the journal committed).
func (j *spoolJournal) poll(t *testing.T, spool, crash string) error {
	t.Helper()
	got, err := ReadSpool(spool)
	if err != nil {
		t.Fatalf("ReadSpool: %v", err)
	}
	if crash == "read" {
		return errTestCrash
	}
	for _, b := range got.Batches {
		if !j.committed[b.Receipt] {
			j.committed[b.Receipt] = true
			j.texts = append(j.texts, texts(b.Events)...)
		}
	}
	if crash == "commit" {
		return errTestCrash
	}
	return AckSpool(spool, receiptsOf(got)...)
}

func TestReadSpoolReturnsReceiptsAndAckSpoolConsumes(t *testing.T) {
	spool := t.TempDir()
	for _, text := range []string{"one", "two"} {
		if err := spoolText(spool, text); err != nil {
			t.Fatal(err)
		}
	}
	files := spoolJSON(t, spool)
	got, err := ReadSpool(spool)
	if err != nil {
		t.Fatalf("ReadSpool: %v", err)
	}
	if len(got.Batches) != 2 || len(got.Quarantined) != 0 || got.More {
		t.Fatalf("ReadSpool = %+v, want two batches and nothing else", got)
	}
	for _, b := range got.Batches {
		data, ok := files[b.Receipt.Name]
		if !ok {
			t.Fatalf("receipt names %q, not a spool file (have %v)", b.Receipt.Name, files)
		}
		sum := sha256.Sum256(data)
		if b.Receipt.Size != int64(len(data)) || b.Receipt.Digest != "sha256:"+hex.EncodeToString(sum[:]) {
			t.Errorf("receipt %+v does not describe the file's %d bytes", b.Receipt, len(data))
		}
	}
	if got := batchTexts(got.Batches); !slices.Equal(sortedCopy(got), []string{"one", "two"}) {
		t.Fatalf("events = %q, want one and two", got)
	}

	// Reading consumes nothing: a second read finds the same files, under the
	// same receipts.
	again, err := ReadSpool(spool)
	if err != nil || !slices.Equal(receiptsOf(again), receiptsOf(got)) {
		t.Fatalf("second ReadSpool = %+v, %v; want the same receipts", again, err)
	}

	if err := AckSpool(spool, receiptsOf(got)...); err != nil {
		t.Fatalf("AckSpool: %v", err)
	}
	if left := spoolJSON(t, spool); len(left) != 0 {
		t.Fatalf("spool after ack = %v, want empty", left)
	}
	if after, err := ReadSpool(spool); err != nil || len(after.Batches) != 0 {
		t.Fatalf("ReadSpool after ack = %+v, %v; want nothing", after, err)
	}
	// A repeated ack, and an ack of a file that never existed, are no-ops.
	if err := AckSpool(spool, receiptsOf(got)...); err != nil {
		t.Fatalf("repeated AckSpool: %v", err)
	}
	never := got.Batches[0].Receipt
	never.Name = "never-written.json"
	if err := AckSpool(spool, never); err != nil {
		t.Fatalf("AckSpool of a file that never existed: %v", err)
	}
}

func sortedCopy(s []string) []string {
	out := slices.Clone(s)
	slices.Sort(out)
	return out
}

// TestSpoolDurabilityOrder pins the order of the durability boundaries: the
// writer fsyncs its file before the rename and the directory after it, all
// before it reports success, and AckSpool verifies a file before removing it
// and fsyncs the directory before it returns.
func TestSpoolDurabilityOrder(t *testing.T) {
	var trace []string
	orig := spoolCrash
	defer func() { spoolCrash = orig }()
	spoolCrash = func(p string) error {
		trace = append(trace, p)
		return nil
	}
	spool := t.TempDir()
	if err := spoolText(spool, "one"); err != nil {
		t.Fatal(err)
	}
	got, err := ReadSpool(spool)
	if err != nil {
		t.Fatal(err)
	}
	if err := AckSpool(spool, receiptsOf(got)...); err != nil {
		t.Fatal(err)
	}
	want := []string{"write:synced", "write:renamed", "write:committed", "ack:verified", "ack:removed", "ack:committed"}
	if !slices.Equal(trace, want) {
		t.Fatalf("durability boundaries = %q, want %q", trace, want)
	}
}

// TestSpoolCrashConsistency crashes the writer, the consumer and AckSpool at
// each boundary, restarts, and polls again. Every batch the writer reported
// written is committed exactly once — replayed where the crash left it unread,
// deduplicated by receipt where the consumer had already committed it — and
// the spool ends empty.
func TestSpoolCrashConsistency(t *testing.T) {
	cases := []struct {
		name        string
		writeCrash  string // crash the writer of "two" here
		pollCrash   string // crash the consumer here: "read" or "commit"
		ackCrash    string // crash AckSpool here
		lostUnlinks bool   // the crash also lost AckSpool's unsynced deletions
		want        []string
	}{
		{name: "writer: temp file synced, not renamed", writeCrash: "write:synced", want: []string{"one"}},
		{name: "writer: renamed, directory not synced", writeCrash: "write:renamed", want: []string{"one", "two"}},
		{name: "writer: after the file commit", writeCrash: "write:committed", want: []string{"one", "two"}},
		{name: "consumer: after read", pollCrash: "read", want: []string{"one", "two"}},
		{name: "consumer: after its commit", pollCrash: "commit", want: []string{"one", "two"}},
		{name: "ack: verified, not removed", ackCrash: "ack:verified", want: []string{"one", "two"}},
		{name: "ack: removed, directory not synced", ackCrash: "ack:removed", want: []string{"one", "two"}},
		{name: "ack: removed, and the crash lost the removal", ackCrash: "ack:removed", lostUnlinks: true, want: []string{"one", "two"}},
		{name: "ack: after the directory sync", ackCrash: "ack:committed", want: []string{"one", "two"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spool := t.TempDir()
			j := &spoolJournal{committed: map[SpoolReceipt]bool{}}
			if err := spoolText(spool, "one"); err != nil {
				t.Fatal(err)
			}
			if tc.writeCrash != "" {
				restart := crashAt(tc.writeCrash)
				err := spoolText(spool, "two")
				restart()
				if !errors.Is(err, errTestCrash) {
					t.Fatalf("writer crashed at %s returned %v; a crash never reports success", tc.writeCrash, err)
				}
			} else if err := spoolText(spool, "two"); err != nil {
				t.Fatal(err)
			}

			before := spoolJSON(t, spool)
			restart := func() {}
			if tc.ackCrash != "" {
				restart = crashAt(tc.ackCrash)
			}
			err := j.poll(t, spool, tc.pollCrash)
			restart()
			crashing := tc.pollCrash != "" || tc.ackCrash != ""
			if crashing && !errors.Is(err, errTestCrash) || !crashing && err != nil {
				t.Fatalf("poll returned %v", err)
			}
			if tc.lostUnlinks {
				for name, data := range before {
					if _, err := os.Lstat(filepath.Join(spool, name)); errors.Is(err, os.ErrNotExist) {
						if err := os.WriteFile(filepath.Join(spool, name), data, 0o600); err != nil {
							t.Fatal(err)
						}
					}
				}
			}

			// Restart: poll until the spool is drained.
			for range 2 {
				if err := j.poll(t, spool, ""); err != nil {
					t.Fatalf("poll after restart: %v", err)
				}
			}
			if !slices.Equal(sortedCopy(j.texts), tc.want) {
				t.Fatalf("committed %q, want each of %q exactly once", j.texts, tc.want)
			}
			if left := spoolJSON(t, spool); len(left) != 0 {
				t.Fatalf("spool after the restart = %v, want empty", left)
			}
		})
	}
}

// TestSpoolWriterCrashLeavesNoVisibleFile: a writer that dies before its
// rename leaves only its temp file, which no reader takes for a batch.
func TestSpoolWriterCrashLeavesNoVisibleFile(t *testing.T) {
	spool := t.TempDir()
	restart := crashAt("write:synced")
	err := spoolText(spool, "lost")
	restart()
	if !errors.Is(err, errTestCrash) {
		t.Fatalf("spoolText = %v, want the crash", err)
	}
	entries, _ := os.ReadDir(spool)
	if len(entries) != 1 || !strings.HasSuffix(entries[0].Name(), ".json.tmp") {
		t.Fatalf("spool = %v, want only the temp file", names(entries))
	}
	got, err := ReadSpool(spool)
	if err != nil || len(got.Batches) != 0 || len(got.Quarantined) != 0 {
		t.Fatalf("ReadSpool = %+v, %v; want the temp file ignored", got, err)
	}
}

func names(entries []os.DirEntry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Name()
	}
	return out
}

func TestAckSpoolRefusesUnsafeNames(t *testing.T) {
	parent := t.TempDir()
	spool := filepath.Join(parent, "spool")
	if err := os.MkdirAll(filepath.Join(spool, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := spoolText(spool, "one"); err != nil {
		t.Fatal(err)
	}
	got, err := ReadSpool(spool)
	if err != nil || len(got.Batches) != 1 {
		t.Fatalf("ReadSpool = %+v, %v", got, err)
	}
	valid := got.Batches[0].Receipt
	data := spoolJSON(t, spool)[valid.Name]
	// Files the unsafe names would reach, each holding the very bytes the
	// receipt describes, so only the name stands between them and deletion.
	victims := []string{filepath.Join(parent, "victim.json"), filepath.Join(spool, "sub", "x.json"), filepath.Join(spool, "x.json.tmp")}
	for _, v := range victims {
		if err := os.WriteFile(v, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{
		"", ".", "..", "../victim.json", "sub/x.json", filepath.Join(parent, "victim.json"),
		"x.json.tmp", "x", `sub\x.json`, "x.json\x00", strings.Repeat("a", 256) + ".json",
	} {
		r := valid
		r.Name = name
		if err := AckSpool(spool, r); !errors.Is(err, ErrSpoolReceipt) {
			t.Errorf("AckSpool(%q) = %v, want ErrSpoolReceipt", name, err)
		}
	}
	for _, r := range []SpoolReceipt{
		{Name: valid.Name, Size: valid.Size, Digest: "md5:" + strings.Repeat("0", 32)},
		{Name: valid.Name, Size: valid.Size, Digest: strings.ToUpper(valid.Digest)},
		{Name: valid.Name, Size: -1, Digest: valid.Digest},
		{Name: valid.Name, Size: MaxSpoolFileBytes + 1, Digest: valid.Digest},
	} {
		if err := AckSpool(spool, r); !errors.Is(err, ErrSpoolReceipt) {
			t.Errorf("AckSpool(%+v) = %v, want ErrSpoolReceipt", r, err)
		}
	}
	for _, v := range append(victims, filepath.Join(spool, valid.Name)) {
		if _, err := os.Stat(v); err != nil {
			t.Errorf("%s was deleted by a refused receipt: %v", v, err)
		}
	}
}

func TestAckSpoolRefusesChangedContents(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(old []byte) []byte
	}{
		{"rewritten, same size", func(old []byte) []byte { return []byte(strings.Replace(string(old), `"one"`, `"won"`, 1)) }},
		{"appended to", func(old []byte) []byte { return append(old, ' ') }},
		{"truncated", func(old []byte) []byte { return old[:len(old)-1] }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spool := t.TempDir()
			if err := spoolText(spool, "one"); err != nil {
				t.Fatal(err)
			}
			got, err := ReadSpool(spool)
			if err != nil || len(got.Batches) != 1 {
				t.Fatalf("ReadSpool = %+v, %v", got, err)
			}
			r := got.Batches[0].Receipt
			path := filepath.Join(spool, r.Name)
			old, _ := os.ReadFile(path)
			changed := tc.change(old)
			if err := os.WriteFile(path, changed, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := AckSpool(spool, r); !errors.Is(err, ErrSpoolReceipt) {
				t.Fatalf("AckSpool of a changed file = %v, want ErrSpoolReceipt", err)
			}
			if now, err := os.ReadFile(path); err != nil || string(now) != string(changed) {
				t.Fatalf("the changed file was touched: %q, %v", now, err)
			}
			// The next read takes the file as it is now: a new receipt, which
			// acks — or, when the change broke the JSON, the quarantine.
			again, err := ReadSpool(spool)
			if err != nil {
				t.Fatal(err)
			}
			switch {
			case len(again.Batches) == 1 && again.Batches[0].Receipt != r:
				if err := AckSpool(spool, receiptsOf(again)...); err != nil {
					t.Fatalf("AckSpool of the new receipt: %v", err)
				}
			case len(again.Batches) == 0 && len(again.Quarantined) == 1:
			default:
				t.Fatalf("re-read = %+v, want the changed file under a new receipt", again)
			}
			if left := spoolJSON(t, spool); len(left) != 0 {
				t.Fatalf("spool = %v, want empty", left)
			}
		})
	}
}

func TestAckSpoolRefusesSymlinkSubstitution(t *testing.T) {
	outside := t.TempDir()
	spool := t.TempDir()
	if err := spoolText(spool, "one"); err != nil {
		t.Fatal(err)
	}
	got, err := ReadSpool(spool)
	if err != nil || len(got.Batches) != 1 {
		t.Fatalf("ReadSpool = %+v, %v", got, err)
	}
	r := got.Batches[0].Receipt
	path := filepath.Join(spool, r.Name)
	data, _ := os.ReadFile(path)
	// The same bytes behind a symlink: following it would verify.
	target := filepath.Join(outside, "target.json")
	if err := os.WriteFile(target, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if err := AckSpool(spool, r); !errors.Is(err, ErrSpoolReceipt) {
		t.Fatalf("AckSpool through a symlink = %v, want ErrSpoolReceipt", err)
	}
	if fi, err := os.Lstat(path); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the substituted symlink was removed: %v", err)
	}
	if now, err := os.ReadFile(target); err != nil || string(now) != string(data) {
		t.Fatalf("the symlink's target was touched: %v", err)
	}
}

// TestReadSpoolQuarantinesUntrustedFiles plants every kind of file a hostile
// writer could put in the spool. ReadSpool returns only the genuine batch,
// never reads through a link, never blocks on the FIFO, and moves each
// untrusted file into the quarantine with a reason.
func TestReadSpoolQuarantinesUntrustedFiles(t *testing.T) {
	outside := t.TempDir()
	spool := t.TempDir()
	if err := spoolText(spool, "genuine"); err != nil {
		t.Fatal(err)
	}
	genuine := ""
	for name := range spoolJSON(t, spool) {
		genuine = name
	}
	// Valid spool contents outside the spool, each carrying a canary.
	canary := func(name, text string) string {
		dir := t.TempDir()
		if err := spoolText(dir, text); err != nil {
			t.Fatal(err)
		}
		for _, data := range spoolJSON(t, dir) {
			if err := os.WriteFile(filepath.Join(outside, name), data, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return filepath.Join(outside, name)
	}
	secret := canary("secret.json", "SYMLINK_CANARY")
	linked := canary("linked.json", "HARDLINK_CANARY")
	unsafe := canary("unsafe.json", "UNSAFE_NAME_CANARY")

	mustDo := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	mustDo(os.Symlink(secret, filepath.Join(spool, "escape.json")))
	mustDo(os.Symlink(genuine, filepath.Join(spool, "alias.json")))
	mustDo(os.Link(linked, filepath.Join(spool, "hardlink.json")))
	mustDo(syscall.Mkfifo(filepath.Join(spool, "fifo.json"), 0o600))
	big, err := os.Create(filepath.Join(spool, "oversize.json"))
	mustDo(err)
	mustDo(big.Truncate(MaxSpoolFileBytes + 1)) // sparse: no disk, no read
	mustDo(big.Close())
	mustDo(os.WriteFile(filepath.Join(spool, "malformed.json"), []byte(`{"not":"a batch"`), 0o600))
	data, _ := os.ReadFile(unsafe)
	mustDo(os.WriteFile(filepath.Join(spool, `back\slash.json`), data, 0o600))
	mustDo(os.Mkdir(filepath.Join(spool, "dir.json"), 0o700))
	mustDo(os.WriteFile(filepath.Join(spool, "inflight.json.tmp"), []byte("partial"), 0o600))

	type result struct {
		got SpoolContents
		err error
	}
	done := make(chan result, 1)
	go func() {
		got, err := ReadSpool(spool)
		done <- result{got, err}
	}()
	var got SpoolContents
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("ReadSpool: %v", r.err)
		}
		got = r.got
	case <-time.After(10 * time.Second):
		t.Fatal("ReadSpool blocked (on the FIFO?)")
	}

	if len(got.Batches) != 1 || got.Batches[0].Receipt.Name != genuine || !slices.Equal(batchTexts(got.Batches), []string{"genuine"}) {
		t.Fatalf("batches = %+v, want only the genuine one", got.Batches)
	}
	wantReasons := map[string]string{
		"escape.json":     "a symlink",
		"alias.json":      "a symlink",
		"hardlink.json":   "hard-linked",
		"fifo.json":       "a named pipe",
		"oversize.json":   "over the",
		"malformed.json":  "malformed",
		`back\slash.json`: "not a plain spool file name",
	}
	for _, q := range got.Quarantined {
		want, ok := wantReasons[q.Name]
		if !ok {
			t.Errorf("unexpected quarantine %+v", q)
			continue
		}
		delete(wantReasons, q.Name)
		if !strings.Contains(q.Reason, want) || q.Deleted {
			t.Errorf("quarantine %+v, want a kept file whose reason says %q", q, want)
		}
		if _, err := os.Lstat(filepath.Join(spool, SpoolQuarantineDir, q.Name)); err != nil {
			t.Errorf("%s is not in the quarantine: %v", q.Name, err)
		}
	}
	for name := range wantReasons {
		t.Errorf("%s was not quarantined", name)
	}
	// Only the genuine file, the directory and the temp file stay behind.
	entries, _ := os.ReadDir(spool)
	if got := names(entries); !slices.Equal(got, sortedCopy([]string{genuine, "dir.json", "inflight.json.tmp", SpoolQuarantineDir})) {
		t.Errorf("spool = %q", got)
	}
	for _, f := range []string{secret, linked, unsafe} {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("%s was touched: %v", f, err)
		}
	}
	// The quarantine is out of the next read's way.
	if again, err := ReadSpool(spool); err != nil || len(again.Quarantined) != 0 || len(again.Batches) != 1 {
		t.Fatalf("second ReadSpool = %+v, %v; want just the genuine batch", again, err)
	}
}

// TestReadSpoolFileStopsAtTheBound: a file that grows past the bound after
// ReadSpool's Lstat is read no further than the bound, and quarantined.
func TestReadSpoolFileStopsAtTheBound(t *testing.T) {
	spool := t.TempDir()
	if err := spoolText(spool, "one"); err != nil {
		t.Fatal(err)
	}
	var name string
	for n := range spoolJSON(t, spool) {
		name = n
	}
	root, err := os.OpenRoot(spool)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	fi, err := root.Lstat(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(filepath.Join(spool, name), MaxSpoolFileBytes+1); err != nil {
		t.Fatal(err)
	}
	_, n, reason, err := readSpoolFile(root, name, fi)
	if err != nil || n != MaxSpoolFileBytes+1 || !strings.Contains(reason, "grew past") {
		t.Fatalf("readSpoolFile = %d bytes, %q, %v; want it stopped at the bound and quarantined", n, reason, err)
	}
}

func TestReadSpoolQuarantineIsBounded(t *testing.T) {
	spool := t.TempDir()
	const extra = 3
	for i := range MaxSpoolQuarantine + extra {
		name := filepath.Join(spool, "bad-"+strings.Repeat("x", i)+".json")
		if err := os.WriteFile(name, []byte("not json"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got, err := ReadSpool(spool)
	if err != nil {
		t.Fatalf("ReadSpool: %v", err)
	}
	kept, deleted := 0, 0
	for _, q := range got.Quarantined {
		if q.Deleted {
			deleted++
			if !strings.Contains(q.Reason, "deleted") {
				t.Errorf("a deleted file's reason does not say so: %q", q.Reason)
			}
		} else {
			kept++
		}
	}
	if kept != MaxSpoolQuarantine || deleted != extra {
		t.Fatalf("kept %d and deleted %d, want %d and %d", kept, deleted, MaxSpoolQuarantine, extra)
	}
	held, _ := os.ReadDir(filepath.Join(spool, SpoolQuarantineDir))
	if len(held) != MaxSpoolQuarantine {
		t.Fatalf("the quarantine holds %d files, want %d", len(held), MaxSpoolQuarantine)
	}
	if left := spoolJSON(t, spool); len(left) != 0 {
		t.Fatalf("spool = %v, want every bad file gone", left)
	}
}

func TestReadSpoolRefusesSymlinkedSpoolDir(t *testing.T) {
	target := t.TempDir()
	if err := spoolText(target, "one"); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "spool")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadSpool(link); err == nil || len(got.Batches) != 0 {
		t.Fatalf("ReadSpool through a symlinked dir = %+v, %v; want an error", got, err)
	}
	got, err := ReadSpool(target)
	if err != nil || len(got.Batches) != 1 {
		t.Fatalf("ReadSpool = %+v, %v", got, err)
	}
	if err := AckSpool(link, receiptsOf(got)...); err == nil {
		t.Fatal("AckSpool through a symlinked dir succeeded")
	}
	if len(spoolJSON(t, target)) != 1 {
		t.Fatal("AckSpool through a symlinked dir deleted the file")
	}
}

// TestReadSpoolBoundsOneCall: a call reads at most its byte and file bounds
// (but always at least one file) and reports More; read-then-ack until More
// is false delivers every batch once.
func TestReadSpoolBoundsOneCall(t *testing.T) {
	for _, tc := range []struct {
		name  string
		bytes int64
		files int
		n     int
		calls []int // batches per call
	}{
		{name: "bytes", bytes: 1, files: 1024, n: 3, calls: []int{1, 1, 1}},
		{name: "files", bytes: MaxSpoolFileBytes, files: 2, n: 5, calls: []int{2, 2, 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			origBytes, origFiles := spoolReadBytes, spoolReadFiles
			defer func() { spoolReadBytes, spoolReadFiles = origBytes, origFiles }()
			spoolReadBytes, spoolReadFiles = tc.bytes, tc.files

			spool := t.TempDir()
			var want []string
			for i := range tc.n {
				text := strings.Repeat("t", i+1)
				want = append(want, text)
				if err := spoolText(spool, text); err != nil {
					t.Fatal(err)
				}
			}
			var gotTexts []string
			var calls []int
			for more := true; more; {
				got, err := ReadSpool(spool)
				if err != nil {
					t.Fatalf("ReadSpool: %v", err)
				}
				calls = append(calls, len(got.Batches))
				gotTexts = append(gotTexts, batchTexts(got.Batches)...)
				if err := AckSpool(spool, receiptsOf(got)...); err != nil {
					t.Fatalf("AckSpool: %v", err)
				}
				more = got.More
				if len(calls) > tc.n+1 {
					t.Fatalf("More never cleared: %v", calls)
				}
			}
			if !slices.Equal(calls, tc.calls) {
				t.Errorf("batches per call = %v, want %v", calls, tc.calls)
			}
			if !slices.Equal(sortedCopy(gotTexts), want) {
				t.Errorf("delivered %q, want %q once each", gotTexts, want)
			}
		})
	}
}

// TestReadSpoolLeavesUnreadableFileInPlace: a file ReadSpool cannot open is
// not quarantined — the fault may be the reader's — but stays, named in the
// error, while the other batches are still returned.
func TestReadSpoolLeavesUnreadableFileInPlace(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a file whatever its mode")
	}
	spool := t.TempDir()
	for _, text := range []string{"one", "two"} {
		if err := spoolText(spool, text); err != nil {
			t.Fatal(err)
		}
	}
	var locked string
	for name := range spoolJSON(t, spool) {
		locked = name
		break
	}
	if err := os.Chmod(filepath.Join(spool, locked), 0); err != nil {
		t.Fatal(err)
	}
	got, err := ReadSpool(spool)
	if err == nil || !strings.Contains(err.Error(), locked) {
		t.Fatalf("ReadSpool error = %v, want one naming %s", err, locked)
	}
	if len(got.Batches) != 1 || len(got.Quarantined) != 0 {
		t.Fatalf("ReadSpool = %+v, want the other batch and no quarantine", got)
	}
	if _, err := os.Lstat(filepath.Join(spool, locked)); err != nil {
		t.Fatalf("the unreadable file moved: %v", err)
	}
}

func TestReadSpoolMissingDirIsEmpty(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	got, err := ReadSpool(missing)
	if err != nil || len(got.Batches) != 0 || len(got.Quarantined) != 0 || got.More {
		t.Fatalf("ReadSpool(missing) = %+v, %v; want empty", got, err)
	}
	r := SpoolReceipt{Name: "x.json", Size: 1, Digest: "sha256:" + strings.Repeat("0", 64)}
	if err := AckSpool(missing, r); err != nil {
		t.Fatalf("AckSpool(missing) = %v, want nil", err)
	}
}

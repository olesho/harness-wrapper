package codex

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/internal/mockapi"
	"github.com/olesho/harness-wrapper/pkg/adapter"
	"github.com/olesho/harness-wrapper/pkg/contract"
	"github.com/olesho/harness-wrapper/pkg/contract/conformance"
)

// savedThreads holds, per codex version, a thread that version saved
// (testdata/load/README.md): what the profile must load, its name and its
// goal with it, before it names the version in Descriptor.Load.
const savedThreads = "testdata/load"

// envRecordSaved names the directory a saved thread is recorded in
// (TestCodexRecordsSavedThread).
const envRecordSaved = "HW_RECORD_SAVED_SESSION"

// savedNative is the native state a saved thread was saved with.
func savedNative(t *testing.T, saved conformance.SavedSession) nativeState {
	t.Helper()
	at := contract.RootPath{Root: contract.RootScratch, Path: nativeDir + "/" + saved.SessionID + ".json"}
	for _, f := range saved.Files {
		if f.At == at {
			var st nativeState
			if err := json.Unmarshal(f.Content, &st); err != nil {
				t.Fatalf("%s: %v", at, err)
			}
			return st
		}
	}
	t.Fatalf("the saved thread has no %s", at)
	return nativeState{}
}

// Every version the profile says it loads has a thread that version saved,
// with a name and a goal to lose.
func TestLoadSourcesHaveSavedThreads(t *testing.T) {
	d := Profile{}.Describe()
	if d.Load == nil || len(d.Load.Sources) == 0 {
		t.Fatalf("the profile loads nothing: %+v", d.Load)
	}
	for _, version := range d.Load.Sources {
		saved, err := conformance.ReadSavedSession(filepath.Join(savedThreads, version))
		if err != nil {
			t.Errorf("codex %s is a source the profile names, and no thread it saved is kept: %v", version, err)
			continue
		}
		if h := saved.Source.Harness; h.Name != Name || h.Version != version || saved.Source.Format != adapter.ArchiveFormat || saved.SessionID == "" {
			t.Errorf("%s/%s: saved by %s %s in format %d, thread %q", savedThreads, version, h.Name, h.Version, saved.Source.Format, saved.SessionID)
		}
		if st := savedNative(t, saved); st.Name == "" || st.Goal == nil {
			t.Errorf("%s/%s: the saved thread has %s: nothing a load could be seen to lose", savedThreads, version, st)
		}
		has := map[string]bool{}
		for _, f := range saved.Files {
			has[f.At.Path] = true
		}
		if !has[indexFile] || !has[goalsFile] {
			t.Errorf("%s/%s: the saved thread lacks %s or %s", savedThreads, version, indexFile, goalsFile)
		}
	}
}

// The pinned codex continues each thread a version the profile names as a
// source saved — the saved files, as that version wrote them, restored into
// a fresh environment — and holds its name and its goal there: the open of a
// loaded thread checks both against what was saved.
func TestCodexLoadsSavedThreads(t *testing.T) {
	bin := realCodex(t)
	mock := mockapi.Start()
	mock.KeepBodies = true
	defer mock.Close()
	f := kitFixture(distribution(t, bin), mock)
	for _, version := range (Profile{}).Describe().Load.Sources {
		t.Run(version, func(t *testing.T) {
			saved, err := conformance.ReadSavedSession(filepath.Join(savedThreads, version))
			if err != nil {
				t.Fatal(err)
			}
			conformance.LoadSaved(conformance.Testing(t), f, saved)
		})
	}
}

// Records the thread the pinned codex saves, as testdata/load/<version>, when
// HW_RECORD_SAVED_SESSION names the directory to record it in: a path the
// saved files will carry, so one that says nothing of the machine. The thread
// has a name and a paused goal. A version's saved thread is recorded once, by
// that version, and never again.
func TestCodexRecordsSavedThread(t *testing.T) {
	base := os.Getenv(envRecordSaved)
	if base == "" {
		t.Skip(envRecordSaved + " does not name a directory to record in")
	}
	bin := realCodex(t)
	mock := mockapi.Start()
	defer mock.Close()
	version := Profile{}.Describe().Harness.Version
	dir := filepath.Join(savedThreads, version)
	if _, err := os.Stat(dir); err == nil {
		t.Fatalf("%s exists: a version's saved thread is never recorded again", dir)
	}
	// The distribution sits beside the agent's roots: the saved files name
	// it too, where the harness recorded a command it ran.
	dist := distributionAt(t, bin, base+"-harness")
	defer func() { _ = os.RemoveAll(base + "-harness") }()
	saved, ok := conformance.RecordSavedSession(conformance.Testing(t), kitFixture(dist, mock), base, func(s contract.Session) {
		tr := codexOf(t, s)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		id := tr.SessionID()
		if _, err := tr.call(ctx, "thread/name/set", map[string]any{"threadId": id, "name": "a saved thread"}); err != nil {
			t.Fatal(err)
		}
		if _, err := tr.call(ctx, "thread/goal/set", map[string]any{"threadId": id, "objective": "Keep what was saved", "status": "paused"}); err != nil {
			t.Fatal(err)
		}
	})
	if !ok {
		t.Fatal("no thread saved")
	}
	if st := savedNative(t, saved); st.Name != "a saved thread" || st.Goal == nil || st.Goal.Status != "paused" {
		t.Fatalf("the thread was saved with %s", st)
	}
	if err := conformance.WriteSavedSession(dir, saved); err != nil {
		t.Fatal(err)
	}
}

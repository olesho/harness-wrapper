package claudecode

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/olesho/harness-wrapper/internal/mockapi"
	"github.com/olesho/harness-wrapper/pkg/adapter"
	"github.com/olesho/harness-wrapper/pkg/contract/conformance"
)

// savedSessions holds, per claude version, a Session that version saved
// (testdata/load/README.md): what the profile must load before it names the
// version in Descriptor.Load.
const savedSessions = "testdata/load"

// envRecordSaved names the directory a saved Session is recorded in
// (TestClaudeRecordsSavedSession).
const envRecordSaved = "HW_RECORD_SAVED_SESSION"

// Every version the profile says it loads has a Session that version saved,
// and the fixture says so itself.
func TestLoadSourcesHaveSavedSessions(t *testing.T) {
	d := Profile{}.Describe()
	if d.Load == nil || len(d.Load.Sources) == 0 {
		t.Fatalf("the profile loads nothing: %+v", d.Load)
	}
	for _, version := range d.Load.Sources {
		saved, err := conformance.ReadSavedSession(filepath.Join(savedSessions, version))
		if err != nil {
			t.Errorf("claude %s is a source the profile names, and no Session it saved is kept: %v", version, err)
			continue
		}
		if h := saved.Source.Harness; h.Name != Name || h.Version != version || saved.Source.Format != adapter.ArchiveFormat || saved.SessionID == "" || len(saved.Files) == 0 {
			t.Errorf("%s/%s: saved by %s %s in format %d, session %q, %d files", savedSessions, version, h.Name, h.Version, saved.Source.Format, saved.SessionID, len(saved.Files))
		}
	}
	kept, err := os.ReadDir(savedSessions)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range kept {
		if e.IsDir() && !d.Loads(adapter.ArchiveFormat, e.Name()) {
			t.Logf("a Session claude %s saved is kept, and the profile does not name that version a source", e.Name())
		}
	}
}

// The pinned claude continues each Session a version the profile names as a
// source saved: the saved files, as that version wrote them, restored into a
// fresh environment.
func TestClaudeLoadsSavedSessions(t *testing.T) {
	bin := realClaude(t)
	mock := mockapi.Start()
	mock.KeepBodies = true
	defer mock.Close()
	f := kitFixture(distribution(t, bin), mock)
	for _, version := range (Profile{}).Describe().Load.Sources {
		t.Run(version, func(t *testing.T) {
			saved, err := conformance.ReadSavedSession(filepath.Join(savedSessions, version))
			if err != nil {
				t.Fatal(err)
			}
			conformance.LoadSaved(conformance.Testing(t), f, saved)
		})
	}
}

// Records the Session the pinned claude saves, as testdata/load/<version>,
// when HW_RECORD_SAVED_SESSION names the directory to record it in: a path
// the saved files will carry, so one that says nothing of the machine. A
// version's saved Session is recorded once, by that version, and never again.
func TestClaudeRecordsSavedSession(t *testing.T) {
	base := os.Getenv(envRecordSaved)
	if base == "" {
		t.Skip(envRecordSaved + " does not name a directory to record in")
	}
	bin := realClaude(t)
	mock := mockapi.Start()
	defer mock.Close()
	version := Profile{}.Describe().Harness.Version
	dir := filepath.Join(savedSessions, version)
	if _, err := os.Stat(dir); err == nil {
		t.Fatalf("%s exists: a version's saved Session is never recorded again", dir)
	}
	// The distribution sits beside the agent's roots: the saved files name
	// it too, where the harness recorded a command it ran.
	dist := distributionAt(t, bin, base+"-harness")
	defer func() { _ = os.RemoveAll(base + "-harness") }()
	saved, ok := conformance.RecordSavedSession(conformance.Testing(t), kitFixture(dist, mock), base, nil)
	if !ok {
		t.Fatal("no Session saved")
	}
	if err := conformance.WriteSavedSession(dir, saved); err != nil {
		t.Fatal(err)
	}
}

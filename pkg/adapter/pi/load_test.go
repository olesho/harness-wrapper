package pi

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/olesho/harness-wrapper/internal/mockapi"
	"github.com/olesho/harness-wrapper/pkg/adapter"
	"github.com/olesho/harness-wrapper/pkg/contract"
	"github.com/olesho/harness-wrapper/pkg/contract/conformance"
)

// savedSessions holds, per pi version, a Session that version saved
// (testdata/load/README.md): what the profile must load before it names the
// version in Descriptor.Load.
const savedSessions = "testdata/load"

// envRecordSaved names the directory a saved Session is recorded in
// (TestPiRecordsSavedSession).
const envRecordSaved = "HW_RECORD_SAVED_SESSION"

// loadModel is the model the saved Sessions run on.
const loadModel = "openai/gpt-4.1-mini"

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
			t.Errorf("pi %s is a source the profile names, and no Session it saved is kept: %v", version, err)
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
			t.Logf("a Session pi %s saved is kept, and the profile does not name that version a source", e.Name())
		}
	}
}

// A load rewrites the session header's working directory, from the
// source's to the new workspace, and nothing when they are one; a request
// that loads nothing rewrites nothing.
func TestProvisionRewritesTheHeader(t *testing.T) {
	a := adapter.New(Profile{})
	d := a.Describe()
	l := contract.Layout{Home: "/n/home", Config: "/n/config", Workspace: "/n/work", Secrets: "/n/secrets", Scratch: "/n/scratch"}
	req := contract.ProvisionRequest{Contract: contract.Version, HarnessRoot: "/h", Layout: l, Spec: contract.AgentSpec{Model: loadModel, PermissionPosture: contract.PostureBypass}}
	res, err := a.Provision(req)
	if err != nil || len(res.HistoryRewrites) != 0 {
		t.Fatalf("no load: %+v %v", res.HistoryRewrites, err)
	}
	req.Load = &contract.LoadSource{
		Format: adapter.ArchiveFormat, Harness: d.Harness, Workspace: "/old/work",
		Layout: contract.Layout{Home: "/old/home", Config: "/old/config", Workspace: "/old/work", Secrets: "/old/secrets", Scratch: "/old/scratch"},
	}
	res, err = a.Provision(req)
	want := []contract.Rewrite{{Path: contract.RootPath{Root: contract.RootConfig, Path: sessionsDir}, Field: "cwd", From: "/old/work", To: "/n/work"}}
	if err != nil || len(res.HistoryRewrites) != 1 || res.HistoryRewrites[0] != want[0] || len(res.HistoryRelocations) != 0 {
		t.Fatalf("a load: rewrites %+v, relocations %+v, %v", res.HistoryRewrites, res.HistoryRelocations, err)
	}
	header := []byte(`{"type":"session","version":3,"id":"s","timestamp":"2026-10-09T00:00:00.000Z","cwd":"/old/work"}`)
	got, ok := contract.RewriteFirstLine(res.HistoryRewrites, contract.RootPath{Root: contract.RootConfig, Path: sessionsDir + "/t_s.jsonl"}, header)
	if !ok || string(got) != `{"type":"session","version":3,"id":"s","timestamp":"2026-10-09T00:00:00.000Z","cwd":"/n/work"}` {
		t.Errorf("the header rewritten %v: %s", ok, got)
	}
	req.Load.Workspace = l.Workspace
	if res, err = a.Provision(req); err != nil || len(res.HistoryRewrites) != 0 {
		t.Errorf("the same workspace: %+v %v, want no rewrite", res.HistoryRewrites, err)
	}
}

// The pinned pi continues each Session a version the profile names as a
// source saved: the saved files, as that version wrote them, restored into a
// fresh environment.
func TestPiLoadsSavedSessions(t *testing.T) {
	bin := realPi(t)
	mock := mockapi.Start()
	mock.KeepBodies = true
	defer mock.Close()
	f := kitFixture(distribution(t, bin), loadModel, mock)
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

// Records the Session the pinned pi saves, as testdata/load/<version>, when
// HW_RECORD_SAVED_SESSION names the directory to record it in: a path the
// saved files will carry, so one that says nothing of the machine. A
// version's saved Session is recorded once, by that version, and never again.
func TestPiRecordsSavedSession(t *testing.T) {
	base := os.Getenv(envRecordSaved)
	if base == "" {
		t.Skip(envRecordSaved + " does not name a directory to record in")
	}
	bin := realPi(t)
	mock := mockapi.Start()
	defer mock.Close()
	version := Profile{}.Describe().Harness.Version
	dir := filepath.Join(savedSessions, version)
	if _, err := os.Stat(dir); err == nil {
		t.Fatalf("%s exists: a version's saved Session is never recorded again", dir)
	}
	dist := distributionAt(t, bin, base+"-harness")
	defer func() { _ = os.RemoveAll(base + "-harness") }()
	saved, ok := conformance.RecordSavedSession(conformance.Testing(t), kitFixture(dist, loadModel, mock), base, nil)
	if !ok {
		t.Fatal("no Session saved")
	}
	if err := conformance.WriteSavedSession(dir, saved); err != nil {
		t.Fatal(err)
	}
}

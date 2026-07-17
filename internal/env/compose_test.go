package env

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

// fakeWorkspace records every call so Compose's operation mapping can be
// asserted without touching a real machine.
type fakeWorkspace struct {
	execCalls     [][]string
	execOpts      []*ExecOpts
	execResult    ExecResult
	execErr       error
	execResultFor map[string]ExecResult // keyed by argv[0]

	uploads   [][2]string
	downloads [][2]string

	tmpPath string

	destroyOutcomes []Outcome
	destroyErr      error
}

func (f *fakeWorkspace) Exec(_ context.Context, argv []string, opts *ExecOpts) (ExecResult, error) {
	f.execCalls = append(f.execCalls, argv)
	f.execOpts = append(f.execOpts, opts)
	if f.execErr != nil {
		return ExecResult{}, f.execErr
	}
	if len(argv) > 0 && f.execResultFor != nil {
		if r, ok := f.execResultFor[argv[0]]; ok {
			return r, nil
		}
	}
	return f.execResult, nil
}

func (f *fakeWorkspace) Upload(_ context.Context, hostPath, guestPath string) error {
	f.uploads = append(f.uploads, [2]string{hostPath, guestPath})
	return nil
}

func (f *fakeWorkspace) Download(_ context.Context, guestPath, hostPath string) error {
	f.downloads = append(f.downloads, [2]string{guestPath, hostPath})
	return nil
}

func (f *fakeWorkspace) GuestPath(kind PathKind) string {
	if kind == PathTmp && f.tmpPath != "" {
		return f.tmpPath
	}
	return "/inner/" + string(kind)
}

func (f *fakeWorkspace) HostAlias(hostURL string) string {
	return "inner(" + hostURL + ")"
}

func (f *fakeWorkspace) Destroy(_ context.Context, outcome Outcome) error {
	f.destroyOutcomes = append(f.destroyOutcomes, outcome)
	return f.destroyErr
}

// fakeLayer is a fully configurable ContainmentLayer for exercising Compose.
type fakeLayer struct {
	execWrapPrefix []string
	crossUpload    []string
	crossDownload  []string
	pathMap        map[PathKind]string
	teardown       []string
}

func (l fakeLayer) ExecWrap(argv []string, opts ExecOpts) ([]string, ExecOpts) {
	return append(append([]string{}, l.execWrapPrefix...), argv...), opts
}
func (l fakeLayer) CrossUpload(_, _ string) []string   { return l.crossUpload }
func (l fakeLayer) CrossDownload(_, _ string) []string { return l.crossDownload }
func (l fakeLayer) PathMap(kind PathKind) string       { return l.pathMap[kind] }
func (l fakeLayer) Teardown() []string                 { return l.teardown }

// fakeLayerWithAlias adds the optional AliasMapper capability.
type fakeLayerWithAlias struct {
	fakeLayer
	alias func(string) string
}

func (l fakeLayerWithAlias) AliasMap(hostURL string) string { return l.alias(hostURL) }

func TestComposeExecWraps(t *testing.T) {
	inner := &fakeWorkspace{execResult: ExecResult{Code: 7, Stdout: "out"}}
	layer := fakeLayer{execWrapPrefix: []string{"wrap", "--"}}
	ws := Compose(inner, layer)

	stdin := "data"
	res, err := ws.Exec(context.Background(), []string{"echo", "hi"}, &ExecOpts{Cwd: "/x", Stdin: &stdin})
	if err != nil {
		t.Fatal(err)
	}
	if res.Code != 7 {
		t.Fatalf("result not passed through: %+v", res)
	}
	want := []string{"wrap", "--", "echo", "hi"}
	if !reflect.DeepEqual(inner.execCalls[0], want) {
		t.Fatalf("execWrap not applied: got %v want %v", inner.execCalls[0], want)
	}
	if inner.execOpts[0] == nil || inner.execOpts[0].Cwd != "/x" {
		t.Fatalf("opts not forwarded: %+v", inner.execOpts[0])
	}
}

func TestComposeUploadIdentity(t *testing.T) {
	// Empty crossUpload ⇒ no boundary ⇒ straight upload to the final guest path.
	inner := &fakeWorkspace{}
	ws := Compose(inner, fakeLayer{})
	if err := ws.Upload(context.Background(), "/host/f", "/guest/f"); err != nil {
		t.Fatal(err)
	}
	if len(inner.uploads) != 1 || inner.uploads[0] != [2]string{"/host/f", "/guest/f"} {
		t.Fatalf("identity upload wrong: %v", inner.uploads)
	}
	if len(inner.execCalls) != 0 {
		t.Fatalf("identity upload should not exec: %v", inner.execCalls)
	}
}

func TestComposeUploadCrossesBoundary(t *testing.T) {
	inner := &fakeWorkspace{tmpPath: "/inner/tmp"}
	layer := fakeLayer{crossUpload: []string{"cross-in"}}
	ws := Compose(inner, layer)
	if err := ws.Upload(context.Background(), "/host/f", "/guest/f"); err != nil {
		t.Fatal(err)
	}
	// Upload lands on a staging path (not the final path), then a cross exec runs.
	if len(inner.uploads) != 1 {
		t.Fatalf("expected 1 staging upload, got %v", inner.uploads)
	}
	if inner.uploads[0][1] == "/guest/f" {
		t.Fatalf("upload should target a staging path, not the final path: %v", inner.uploads[0])
	}
	if len(inner.execCalls) != 1 || inner.execCalls[0][0] != "cross-in" {
		t.Fatalf("cross exec not run: %v", inner.execCalls)
	}
}

func TestComposeDownloadCrossesBoundary(t *testing.T) {
	inner := &fakeWorkspace{tmpPath: "/inner/tmp"}
	layer := fakeLayer{crossDownload: []string{"cross-out"}}
	ws := Compose(inner, layer)
	if err := ws.Download(context.Background(), "/guest/f", "/host/f"); err != nil {
		t.Fatal(err)
	}
	// The cross exec runs first, then the file is pulled from staging to host.
	if len(inner.execCalls) != 1 || inner.execCalls[0][0] != "cross-out" {
		t.Fatalf("cross download exec not run: %v", inner.execCalls)
	}
	if len(inner.downloads) != 1 || inner.downloads[0][1] != "/host/f" {
		t.Fatalf("staging download wrong: %v", inner.downloads)
	}
}

func TestComposeCrossUploadPropagatesExitFailure(t *testing.T) {
	inner := &fakeWorkspace{execResult: ExecResult{Code: 3, Stderr: "nope"}}
	layer := fakeLayer{crossUpload: []string{"cross-in"}}
	ws := Compose(inner, layer)
	err := ws.Upload(context.Background(), "/host/f", "/guest/f")
	if err == nil {
		t.Fatal("expected error from non-zero cross exec")
	}
}

func TestComposeGuestPathShadowing(t *testing.T) {
	inner := &fakeWorkspace{}
	layer := fakeLayer{pathMap: map[PathKind]string{PathRepo: "/contained/repo"}}
	ws := Compose(inner, layer)
	if got := ws.GuestPath(PathRepo); got != "/contained/repo" {
		t.Fatalf("shadow path not used: %q", got)
	}
	// Unmapped kinds defer to the inner path.
	if got := ws.GuestPath(PathHome); got != "/inner/home" {
		t.Fatalf("unmapped kind should defer to inner: %q", got)
	}
}

func TestComposeHostAliasTwoHop(t *testing.T) {
	inner := &fakeWorkspace{}
	// Without an AliasMapper, only the inner hop applies.
	plain := Compose(inner, fakeLayer{})
	if got := plain.HostAlias("http://h"); got != "inner(http://h)" {
		t.Fatalf("plain alias wrong: %q", got)
	}
	// With an AliasMapper, both hops fold.
	twohop := Compose(inner, fakeLayerWithAlias{
		alias: func(s string) string { return "contain(" + s + ")" },
	})
	if got := twohop.HostAlias("http://h"); got != "contain(inner(http://h))" {
		t.Fatalf("two-hop alias wrong: %q", got)
	}
}

func TestComposeDestroyOrderAndAggregation(t *testing.T) {
	inner := &fakeWorkspace{}
	layer := fakeLayer{teardown: []string{"teardown-cmd"}}
	ws := Compose(inner, layer)
	if err := ws.Destroy(context.Background(), OutcomeSuccess); err != nil {
		t.Fatal(err)
	}
	// Teardown runs via inner exec, then inner is destroyed with the outcome.
	if len(inner.execCalls) != 1 || inner.execCalls[0][0] != "teardown-cmd" {
		t.Fatalf("teardown not run via inner exec: %v", inner.execCalls)
	}
	if len(inner.destroyOutcomes) != 1 || inner.destroyOutcomes[0] != OutcomeSuccess {
		t.Fatalf("inner destroy not called with outcome: %v", inner.destroyOutcomes)
	}
}

func TestComposeDestroyAggregatesBothFailures(t *testing.T) {
	// Both the teardown exec AND inner destroy fail — errors must aggregate,
	// never short-circuit.
	inner := &fakeWorkspace{
		execResult: ExecResult{Code: 1, Stderr: "teardown boom"},
		destroyErr: errors.New("inner boom"),
	}
	layer := fakeLayer{teardown: []string{"teardown-cmd"}}
	ws := Compose(inner, layer)
	err := ws.Destroy(context.Background(), OutcomeFailure)
	var te *TeardownError
	if !errors.As(err, &te) {
		t.Fatalf("expected TeardownError, got %v", err)
	}
	if len(te.Errors) != 2 {
		t.Fatalf("expected 2 aggregated errors, got %d: %v", len(te.Errors), te.Errors)
	}
	// Inner destroy still ran despite the teardown failure.
	if len(inner.destroyOutcomes) != 1 {
		t.Fatalf("inner destroy should still run: %v", inner.destroyOutcomes)
	}
}

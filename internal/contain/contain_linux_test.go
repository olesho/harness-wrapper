//go:build linux

package contain

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/internal/landlock"
	"github.com/olesho/harness-wrapper/internal/testsupport/cfd"
	"github.com/olesho/harness-wrapper/pkg/containment"
	"golang.org/x/sys/unix"
)

var landlockProbe = func() (int, error) { return landlock.Probe(0) }

// testHarness registers a profile for the test binary and isolates managed
// state in a per-test directory.
func testHarness(t *testing.T) (self string) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	self, _ = filepath.EvalSymlinks(self)
	restore := RegisterTestProfile(TestProfile{Harness: "hwtest", ExecDirs: []string{filepath.Dir(self)}})
	t.Cleanup(restore)
	// A short state root: the claude profile's TMPDIR limit is not in play for
	// the test profile, but keep paths realistic.
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	return self
}

func helperInput(t *testing.T, self, wd string, req *containment.Request, args ...string) Input {
	t.Helper()
	req.PassEnv = append(req.PassEnv, helperEnv, "GODEBUG")
	env := append(os.Environ(), helperEnv+"=report", "GODEBUG=containermaxprocs=0")
	return Input{
		Request:    req,
		Harness:    "hwtest",
		BinaryPath: self,
		Args:       args,
		WorkingDir: wd,
		Env:        env,
	}
}

// runContained prepares, starts and finishes one contained helper run.
func runContained(t *testing.T, in Input) (*containment.Applied, *os.ProcessState, string) {
	t.Helper()
	return runContainedWith(t, in, nil)
}

// runContainedWith is runContained with beforeStart run after the policy is
// validated and its rules installed (Prepare) and before it is enforced
// (Start).
func runContainedWith(t *testing.T, in Input, beforeStart func()) (*containment.Applied, *os.ProcessState, string) {
	t.Helper()
	l, err := Prepare(in)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if beforeStart != nil {
		beforeStart()
	}
	return startPrepared(t, l)
}

// startPrepared starts a prepared launch on a fresh terminal, waits for the
// helper and finishes the session.
func startPrepared(t *testing.T, l *Launch) (*containment.Applied, *os.ProcessState, string) {
	t.Helper()
	master, slave, err := OpenPTYPair()
	if err != nil {
		l.Release()
		t.Fatal(err)
	}
	mf := masterFile(master)
	defer func() { _ = mf.Close() }()
	if err := l.AddTerminal(slave); err != nil {
		_ = unix.Close(slave)
		l.Release()
		t.Fatal(err)
	}
	pid, err := l.Start(slave)
	_ = unix.Close(slave)
	if err != nil {
		l.Release()
		t.Fatalf("Start: %v", err)
	}
	done := drain(mf)
	st := waitPID(t, pid)
	cleanup := l.Finish(pid, false, time.Now().Add(2*time.Second))
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Error("PTY master did not reach EOF after the session ended")
	}
	return l.Applied(), st, cleanup
}

func TestRefusals(t *testing.T) {
	self := testHarness(t)
	wd := t.TempDir()
	base := func() *containment.Request { return &containment.Request{Kind: containment.KindLandlock} }
	parent, err := StateParent()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(parent, 0o700); err != nil {
		t.Fatal(err)
	}

	// Stages before the kernel check refuse on any kernel; the path checks
	// run only once a kernel that could enforce has been found.
	kernelOK := false
	if _, err := landlockProbe(); err == nil {
		kernelOK = true
	}
	cases := []struct {
		name  string
		in    func() Input
		stage string
	}{
		{"unknown kind", func() Input {
			in := helperInput(t, self, wd, base())
			in.Request.Kind = "seatbelt"
			return in
		}, StageRequest},
		{"ports without restriction", func() Input {
			r := base()
			r.ConnectTCP = []uint16{443}
			return helperInput(t, self, wd, r)
		}, StageRequest},
		{"no profile", func() Input {
			in := helperInput(t, self, wd, base())
			in.Harness = "opencode"
			return in
		}, StageProfile},
		{"inactive profile", func() Input {
			t.Cleanup(RegisterTestProfile(TestProfile{Harness: "hwtest-inactive", Inactive: true}))
			in := helperInput(t, self, wd, base())
			in.Harness = "hwtest-inactive"
			return in
		}, StageProfile},
		{"codex below bypass", func() Input {
			restore := ActivateProfilesForTest()
			t.Cleanup(restore)
			in := helperInput(t, self, wd, base())
			in.Harness, in.LaunchRung = "codex", "ask"
			return in
		}, StageProfile},
		{"missing grant", func() Input {
			r := base()
			r.ReadOnly = []string{filepath.Join(wd, "does-not-exist")}
			return helperInput(t, self, wd, r)
		}, StagePaths},
		{"read-only inside writable", func() Input {
			sub := filepath.Join(wd, "sub")
			_ = os.MkdirAll(sub, 0o700)
			r := base()
			r.ReadOnly = []string{sub}
			return helperInput(t, self, wd, r)
		}, StagePaths},
		{"grant exposes managed state", func() Input {
			r := base()
			r.ReadOnly = []string{filepath.Dir(parent)}
			return helperInput(t, self, wd, r)
		}, StagePaths},
		{"grant inside managed state", func() Input {
			r := base()
			r.ReadWrite = []string{parent}
			return helperInput(t, self, wd, r)
		}, StagePaths},
		{"writable cgroupfs", func() Input {
			r := base()
			r.ReadWrite = []string{"/sys/fs/cgroup"}
			return helperInput(t, self, wd, r)
		}, StagePaths},
	}
	if _, err := landlockProbe(); err == nil {
		cases = append(cases, struct {
			name  string
			in    func() Input
			stage string
		}{"abi too high", func() Input {
			r := base()
			r.MinABI = 99
			return helperInput(t, self, wd, r)
		}, StageKernel})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.stage == StagePaths && !kernelOK {
				t.Skip("path checks follow the kernel check, which refuses first here")
			}
			marker := filepath.Join(wd, "started-"+strings.ReplaceAll(tc.name, " ", "-"))
			in := tc.in()
			in.Args = []string{marker}
			l, err := Prepare(in)
			if err == nil {
				l.Release()
				t.Fatal("Prepare accepted a request it must refuse")
			}
			var re *RefusalError
			if !errors.As(err, &re) || !errors.Is(err, ErrRefused) {
				t.Fatalf("error %v is not a refusal", err)
			}
			if re.Stage != tc.stage {
				t.Fatalf("refused at stage %q, want %q: %v", re.Stage, tc.stage, err)
			}
			if _, err := os.Stat(marker); err == nil {
				t.Fatal("a refused launch started the harness")
			}
		})
	}
	// Refusals leave no managed state behind.
	if ents, _ := os.ReadDir(parent); len(ents) != 0 {
		t.Fatalf("refusals left %d managed-state entries", len(ents))
	}
}

func TestPinnedObjects(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	if err := os.MkdirAll(filepath.Join(real, "child"), 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	pl, err := pinPath(link)
	if err != nil {
		t.Fatal(err)
	}
	defer pl.close()
	canonical, _ := filepath.EvalSymlinks(real)
	if pl.canonical != canonical || !pl.isDir() {
		t.Fatalf("pinned %q as %q (dir=%v), want %q", link, pl.canonical, pl.isDir(), canonical)
	}
	pc, err := pinPath(filepath.Join(link, "child"))
	if err != nil {
		t.Fatal(err)
	}
	defer pc.close()
	if !pl.contains(pc) || pc.contains(pl) || !pc.overlaps(pl) {
		t.Fatal("ancestry through a symlink was not resolved by object identity")
	}
	// Replacing the path after pinning never redirects the pinned object.
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(dir, "other")
	_ = os.Mkdir(other, 0o700)
	_ = os.Symlink(other, link)
	var st unix.Stat_t
	if err := unix.Fstat(pl.fd, &st); err != nil || st.Ino != pl.id.ino {
		t.Fatal("pinned descriptor changed identity")
	}
	if _, err := pinPath(filepath.Join(dir, "missing")); !errors.Is(err, errNotFound) {
		t.Fatalf("missing path: %v", err)
	}
}

// TestDescriptorContract checks the private-table spawn without a domain, so
// it runs on any kernel: descriptors the wrapper holds without close-on-exec —
// a file and a listener — never reach the child, and the wrapper's own table
// and flags do not change.
func TestDescriptorContract(t *testing.T) {
	self, _ := os.Executable()
	dir := t.TempDir()
	secret := filepath.Join(dir, "secret")
	if err := os.WriteFile(secret, []byte("s3cret"), 0o600); err != nil {
		t.Fatal(err)
	}
	planted, err := syscall.Open(secret, syscall.O_RDONLY, 0) // no O_CLOEXEC
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = syscall.Close(planted) }()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	lf, _ := ln.(*net.TCPListener).File()
	defer func() { _ = lf.Close() }()
	if _, err := unix.FcntlInt(lf.Fd(), unix.F_SETFD, 0); err != nil {
		t.Fatal(err)
	}
	var planted2 []int
	if cfd.Available {
		for _, fd := range []int{cfd.OpenNonCloexec(secret), cfd.ListenNonCloexec()} {
			if fd < 0 {
				t.Fatal("planting C descriptors failed")
			}
			planted2 = append(planted2, fd)
			t.Cleanup(func() { cfd.Close(fd) })
		}
	}

	before := fdState(t)
	for i := 0; i < 20; i++ {
		report := filepath.Join(dir, "report-"+strconv.Itoa(i)+".json")
		master, slave, err := OpenPTYPair()
		if err != nil {
			t.Fatal(err)
		}
		env := append(os.Environ(), helperEnv+"=report", "GODEBUG=containermaxprocs=0")
		r := spawn(spawnSpec{path: self, argv: []string{self, report}, env: env, dir: dir, tty: slave, cgroupFD: -1})
		_ = unix.Close(slave)
		if r.err != nil {
			t.Fatalf("spawn: %s: %v", r.stage, r.err)
		}
		mf := masterFile(master)
		done := drain(mf)
		if st := waitPID(t, r.pid); !st.Success() {
			t.Fatalf("child: %v", st)
		}
		<-done
		_ = mf.Close()
		rep := readHelperReport(t, report)
		if l := leaked(rep); len(l) > 0 {
			t.Fatalf("child inherited %v (planted %d, %d, %v)", l, planted, lf.Fd(), planted2)
		}
		if !strings.HasPrefix(rep.FDs["0"], "/dev/pts/") {
			t.Fatalf("child stdin is %q, not its terminal", rep.FDs["0"])
		}
	}
	time.Sleep(50 * time.Millisecond)
	after := fdState(t)
	for fd, desc := range before {
		if after[fd] != desc {
			t.Errorf("wrapper descriptor %d changed: %q -> %q", fd, desc, after[fd])
		}
	}
	for fd, desc := range after {
		if _, ok := before[fd]; !ok {
			t.Errorf("wrapper gained descriptor %d (%s)", fd, desc)
		}
	}
}

// TestMainThreadHandOff forces a spawn onto the main thread in a subprocess
// and checks the PTY master still reaches EOF: the parked main thread must
// hold no private table.
func TestMainThreadHandOff(t *testing.T) {
	self, _ := os.Executable()
	dir := t.TempDir()
	cmd := exec.Command(self, dir)
	cmd.Env = append(os.Environ(), helperEnv+"=m0", "GOMAXPROCS=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("m0 helper: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "eof=true") {
		t.Fatalf("m0 helper: %s", out)
	}
	t.Logf("%s", strings.TrimSpace(string(out)))
}

// TestSpawnThreadExitUnderTimerLoad: a spawn thread leaves through the
// runtime's exit path, which may write the netpoller's eventfd after the
// spawn has reported. It runs in a subprocess because a failure kills the
// whole process without a message.
func TestSpawnThreadExitUnderTimerLoad(t *testing.T) {
	self, _ := os.Executable()
	cmd := exec.Command(self, "100000")
	cmd.Env = append(os.Environ(), helperEnv+"=exitrace", "GOMAXPROCS=4", "GODEBUG=containermaxprocs=0")
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "exitrace ok") {
		t.Fatalf("spawns under timer load: %v (a silent exit status 2 is a runtime throw on a spawn thread)\n%s", err, out)
	}
}

// TestEnforcement runs a contained helper that tries each controlled
// operation and reports the errno.
func TestEnforcement(t *testing.T) {
	requireABI(t)
	self := testHarness(t)
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret")
	if err := os.WriteFile(secret, []byte("s3cret"), 0o600); err != nil {
		t.Fatal(err)
	}
	wd := t.TempDir()
	roDir := t.TempDir()
	roFile := filepath.Join(roDir, "readable")
	_ = os.WriteFile(roFile, []byte("r"), 0o600)
	rwDir := t.TempDir()
	rwFile := filepath.Join(rwDir, "existing")
	_ = os.WriteFile(rwFile, []byte("data"), 0o600)
	renamed := filepath.Join(rwDir, "renamed")
	// Symlinks inside a writable grant that point outside every grant.
	escapeFile := filepath.Join(rwDir, "escape")
	escapeDir := filepath.Join(rwDir, "escape-dir")
	if err := os.Symlink(secret, escapeFile); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, escapeDir); err != nil {
		t.Fatal(err)
	}

	sock := filepath.Join(outside, "srv.sock")
	pln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pln.Close() }()
	abstract := fmt.Sprintf("hwc-test-%d", os.Getpid())
	aln, err := net.Listen("unix", "@"+abstract)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = aln.Close() }()
	allowed, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = allowed.Close() }()
	denied, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = denied.Close() }()
	allowedPort := allowed.Addr().(*net.TCPAddr).Port
	deniedPort := denied.Addr().(*net.TCPAddr).Port
	for _, l := range []net.Listener{pln, aln, allowed, denied} {
		go func(l net.Listener) {
			for {
				c, err := l.Accept()
				if err != nil {
					return
				}
				_ = c.Close()
			}
		}(l)
	}

	report := filepath.Join(wd, "report.json")
	req := &containment.Request{
		Kind:        containment.KindLandlock,
		ReadOnly:    []string{roDir},
		ReadWrite:   []string{rwDir},
		RestrictTCP: true,
		ConnectTCP:  []uint16{uint16(allowedPort)},
	}
	// Each check runs in this order in the child; want is its errno.
	cases := []struct{ check, want string }{
		{"read=" + secret, "EACCES"},
		{"write=" + filepath.Join(outside, "new"), "EACCES"},
		{"read=" + roFile, "ok"},
		{"write=" + filepath.Join(roDir, "new"), "EACCES"},
		{"write=" + filepath.Join(rwDir, "new"), "ok"},
		{"mkdir=" + filepath.Join(wd, "made"), "ok"},
		{"truncate=" + roFile, "EACCES"},
		{"truncate=" + rwFile, "ok"},
		{"remove=" + roFile, "EACCES"},
		{"rename=" + rwFile + ":" + renamed, "ok"},
		{"rename=" + renamed + ":" + filepath.Join(outside, "moved"), "EACCES"},
		{"rename=" + renamed + ":" + filepath.Join(roDir, "moved"), "EACCES"},
		{"remove=" + renamed, "ok"},
		{"read=" + escapeFile, "EACCES"},
		{"write=" + filepath.Join(escapeDir, "through-link"), "EACCES"},
		{"connect-unix=" + sock, "EACCES"},
		{"connect-abstract=" + abstract, "EPERM"},
		{"listen-unix=" + filepath.Join(wd, "own.sock"), "ok"},
		{"kill=" + strconv.Itoa(os.Getpid()), "EPERM"},
		{"tcp=" + strconv.Itoa(allowedPort), "ok"},
		{"tcp=" + strconv.Itoa(deniedPort), "EACCES"},
		{"bind=0", "EACCES"},
		// Leaving the cgroup: the domain denies every write under cgroupfs.
		{"cgroup-move", "EACCES"},
		{"cgroup-mkdir", "EACCES"},
	}
	// The user manager's sockets, through which a descendant could have
	// systemd start processes outside the domain and its cgroup.
	for _, s := range []string{"bus", "systemd/private"} {
		p := fmt.Sprintf("/run/user/%d/%s", os.Getuid(), s)
		if fi, err := os.Stat(p); err == nil && fi.Mode()&os.ModeSocket != 0 {
			cases = append(cases, struct{ check, want string }{"connect-unix=" + p, "EACCES"})
		}
	}
	in := helperInput(t, self, wd, req, append(checkArgs(report, cases), "env", "cwd")...)
	in.Env = append(in.Env, "SECRET_TOKEN=do-not-inherit")
	applied, st, cleanup := runContained(t, in)
	if !st.Success() {
		t.Fatalf("contained helper exited %v", st)
	}
	rep := readHelperReport(t, report)
	checkReport(t, rep, cases)
	if b, err := os.ReadFile(secret); err != nil || string(b) != "s3cret" {
		t.Errorf("the file outside every grant changed: %q, %v", b, err)
	}
	if b, _ := os.ReadFile(roFile); string(b) != "r" {
		t.Errorf("the read-only file changed: %q", b)
	}
	if l := leaked(rep); len(l) > 0 {
		t.Errorf("contained child inherited %v", l)
	}
	canonicalWD, _ := filepath.EvalSymlinks(wd)
	if rep.Cwd != canonicalWD {
		t.Errorf("child cwd %q, want %q", rep.Cwd, canonicalWD)
	}
	for _, kv := range rep.Env {
		if strings.HasPrefix(kv, "SECRET_TOKEN=") {
			t.Error("an unlisted variable reached the contained child")
		}
		if v, ok := strings.CutPrefix(kv, "HOME="); ok && v != applied.State.Home {
			t.Errorf("HOME=%s, want the private %s", v, applied.State.Home)
		}
	}
	// The wrapper itself stays unrestricted.
	if _, err := os.ReadFile(secret); err != nil {
		t.Errorf("the wrapper lost access after a contained spawn: %v", err)
	}
	if applied.Fingerprint == "" || applied.ABI < 9 || applied.TCP.Mode != "restricted" {
		t.Errorf("applied policy incomplete: %+v", applied)
	}
	if applied.Supervision.Mode == containment.SupervisionCgroup && cleanup != cleanupComplete {
		t.Errorf("supervised cleanup: %s", cleanup)
	}
	if applied.Supervision.Mode == containment.SupervisionCgroup {
		if _, err := os.Stat(applied.State.Home); !os.IsNotExist(err) {
			t.Errorf("ephemeral state survived a complete cleanup: %v", err)
		}
	}
	t.Logf("supervision=%s cleanup=%s fingerprint=%s", applied.Supervision.Mode, cleanup, applied.Fingerprint)
}

// checkReport compares a helper's check results with the wanted errno names.
func checkReport(t *testing.T, rep helperReport, cases []struct{ check, want string }) {
	t.Helper()
	for _, c := range cases {
		if got := rep.Checks[c.check]; got != c.want {
			t.Errorf("%s: got %s, want %s", c.check, got, c.want)
		}
	}
}

func checkArgs(report string, cases []struct{ check, want string }) []string {
	args := []string{report}
	for _, c := range cases {
		args = append(args, c.check)
	}
	return args
}

// mkdirWith creates dir with one file in it, named and holding name.
func mkdirWith(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0o600); err != nil {
		t.Fatal(err)
	}
}

// retarget atomically points the symlink link at target.
func retarget(t *testing.T, link, target string) {
	t.Helper()
	tmp := link + ".tmp"
	_ = os.Remove(tmp)
	if err := os.Symlink(target, tmp); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, link); err != nil {
		t.Fatal(err)
	}
}

// TestGrantsBindToValidatedObjects replaces a granted symlink's target and a
// granted directory's ancestor after the policy was validated and its rules
// installed, before it is enforced: the child keeps exactly the objects that
// were validated and gets nothing through their replacements.
func TestGrantsBindToValidatedObjects(t *testing.T) {
	requireABI(t)
	self := testHarness(t)
	dir := t.TempDir()
	real, decoy := filepath.Join(dir, "real"), filepath.Join(dir, "decoy")
	mkdirWith(t, real, "f")
	mkdirWith(t, decoy, "f")
	link := filepath.Join(dir, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(dir, "a", "b")
	mkdirWith(t, nested, "f")
	validated := filepath.Join(dir, "a-validated", "b")
	wd := t.TempDir()
	report := filepath.Join(wd, "report.json")
	cases := []struct{ check, want string }{
		{"read=" + filepath.Join(real, "f"), "ok"},
		{"read=" + filepath.Join(link, "f"), "EACCES"}, // the decoy, by now
		{"read=" + filepath.Join(decoy, "f"), "EACCES"},
		{"write=" + filepath.Join(validated, "new"), "ok"},
		{"write=" + filepath.Join(nested, "new"), "EACCES"}, // the replacement
	}
	req := &containment.Request{Kind: containment.KindLandlock, ReadOnly: []string{link}, ReadWrite: []string{nested}}
	applied, st, _ := runContainedWith(t, helperInput(t, self, wd, req, checkArgs(report, cases)...), func() {
		retarget(t, link, decoy)
		if err := os.Rename(filepath.Join(dir, "a"), filepath.Dir(validated)); err != nil {
			t.Fatal(err)
		}
		mkdirWith(t, nested, "f")
	})
	if !st.Success() {
		t.Fatalf("contained helper exited %v", st)
	}
	checkReport(t, readHelperReport(t, report), cases)
	realC, _ := filepath.EvalSymlinks(real)
	for _, g := range applied.Grants {
		if g.Requested == link && g.Path != realC {
			t.Errorf("the applied policy names %s for %s, not the validated %s", g.Path, link, realC)
		}
	}
}

// TestGrantRaceFailsClosedOrBindsOneObject retargets a granted symlink and
// exchanges a granted directory's ancestor continuously while launches
// validate their grants. Each launch either fails closed or grants exactly
// one object per requested path — for the symlink, the one its applied policy
// names — never both.
func TestGrantRaceFailsClosedOrBindsOneObject(t *testing.T) {
	requireABI(t)
	self := testHarness(t)
	dir := t.TempDir()
	real, decoy := filepath.Join(dir, "real"), filepath.Join(dir, "decoy")
	mkdirWith(t, real, "f")
	mkdirWith(t, decoy, "f")
	a, a2 := filepath.Join(dir, "a"), filepath.Join(dir, "a2")
	mkdirWith(t, filepath.Join(a, "b"), "id-A")
	mkdirWith(t, filepath.Join(a2, "b"), "id-B")
	link := filepath.Join(dir, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	realC, _ := filepath.EvalSymlinks(real)
	decoyC, _ := filepath.EvalSymlinks(decoy)
	idReads := []string{}
	for _, p := range []string{a, a2} {
		for _, id := range []string{"id-A", "id-B"} {
			idReads = append(idReads, "read="+filepath.Join(p, "b", id))
		}
	}

	launched := 0
	for i := 0; i < 20; i++ {
		wd := t.TempDir()
		report := filepath.Join(wd, "report.json")
		args := append([]string{report, "read=" + filepath.Join(realC, "f"), "read=" + filepath.Join(decoyC, "f")}, idReads...)
		req := &containment.Request{Kind: containment.KindLandlock, ReadOnly: []string{link, filepath.Join(a, "b")}}
		in := helperInput(t, self, wd, req, args...)

		stop := make(chan struct{})
		flipped := make(chan struct{})
		go func() {
			defer close(flipped)
			targets := []string{decoy, real}
			for n := 0; ; n++ {
				select {
				case <-stop:
					return
				default:
				}
				tmp := link + ".tmp"
				_ = os.Remove(tmp)
				if os.Symlink(targets[n%2], tmp) == nil {
					_ = os.Rename(tmp, link)
				}
				_ = unix.Renameat2(unix.AT_FDCWD, a, unix.AT_FDCWD, a2, unix.RENAME_EXCHANGE)
			}
		}()
		l, err := Prepare(in)
		close(stop)
		<-flipped
		if err != nil {
			var re *RefusalError
			if !errors.As(err, &re) || re.Stage != StagePaths {
				t.Fatalf("launch %d: %v is not a paths refusal", i, err)
			}
			continue // failed closed
		}
		launched++
		applied, st, _ := startPrepared(t, l)
		if !st.Success() {
			t.Fatalf("launch %d: contained helper exited %v", i, st)
		}
		rep := readHelperReport(t, report)

		var named string
		for _, g := range applied.Grants {
			if g.Requested == link {
				named = g.Path
			}
		}
		other := map[string]string{realC: decoyC, decoyC: realC}[named]
		if other == "" {
			t.Fatalf("launch %d: the symlink's grant names %q", i, named)
		}
		if got := rep.Checks["read="+filepath.Join(named, "f")]; got != "ok" {
			t.Errorf("launch %d: the named object %s: %s", i, named, got)
		}
		if got := rep.Checks["read="+filepath.Join(other, "f")]; got != "EACCES" {
			t.Errorf("launch %d: the other object %s: %s", i, other, got)
		}
		// Each id file exists at one of the two places; exactly one of the
		// two objects is readable.
		counts := map[string]int{}
		for _, c := range idReads {
			counts[rep.Checks[c]]++
		}
		if counts["ENOENT"] != 2 || counts["ok"] != 1 || counts["EACCES"] != 1 {
			t.Errorf("launch %d: ancestor exchange results %v", i, counts)
		}
	}
	t.Logf("%d of 20 launches proceeded; the rest failed closed", launched)
}

// TestPrivateStateIsPerSession: one session's private HOME and TMPDIR are
// outside every other session's domain, while its own are writable.
func TestPrivateStateIsPerSession(t *testing.T) {
	requireABI(t)
	self := testHarness(t)
	// Caller-owned, so its launch keeps it for the second session to probe.
	st, err := NewState(false)
	if err != nil {
		t.Fatal(err)
	}
	supervised := false
	defer func() {
		// Removal needs the launch's cgroup shown empty; after an
		// unsupervised launch the state is kept.
		err := st.Remove(t.Context())
		switch {
		case supervised && err != nil:
			t.Errorf("remove state: %v", err)
		case !supervised && err == nil:
			t.Error("state removed although its unsupervised launch's descendants cannot be shown gone")
		}
		st.Close()
	}()
	wd := t.TempDir()
	report := filepath.Join(wd, "first.json")
	first := []struct{ check, want string }{
		{"write=$HOME/probe", "ok"},
		{"write=$TMPDIR/probe", "ok"},
	}
	in := helperInput(t, self, wd, &containment.Request{Kind: containment.KindLandlock}, checkArgs(report, first)...)
	in.State = st
	a, ps, _ := runContained(t, in)
	supervised = a.Supervision.Mode == containment.SupervisionCgroup
	if !ps.Success() {
		t.Fatalf("first session exited %v", ps)
	}
	checkReport(t, readHelperReport(t, report), first)

	report = filepath.Join(wd, "second.json")
	second := []struct{ check, want string }{
		{"read=" + filepath.Join(a.State.Home, "probe"), "EACCES"},
		{"write=" + filepath.Join(a.State.Home, "planted"), "EACCES"},
		{"read=" + filepath.Join(a.State.Tmp, "probe"), "EACCES"},
		{"read=" + filepath.Join(st.Root(), "lifecycle.json"), "EACCES"},
		{"write=$HOME/probe", "ok"},
	}
	b, ps, _ := runContained(t, helperInput(t, self, wd, &containment.Request{Kind: containment.KindLandlock}, checkArgs(report, second)...))
	if !ps.Success() {
		t.Fatalf("second session exited %v", ps)
	}
	checkReport(t, readHelperReport(t, report), second)
	if b.State.Home == a.State.Home || b.State.ID == a.State.ID {
		t.Fatalf("two sessions share private state %s", a.State.ID)
	}
}

// TestPreviewMatchesApplied: contain-check's fingerprint is the one a launch
// under the same policy reports, and its private paths are placeholders.
func TestPreviewMatchesApplied(t *testing.T) {
	requireABI(t)
	self := testHarness(t)
	wd := t.TempDir()
	extra := t.TempDir()
	req := func() *containment.Request {
		return &containment.Request{Kind: containment.KindLandlock, ReadWrite: []string{extra}, RestrictTCP: true, ConnectTCP: []uint16{443}}
	}
	p, err := PreviewLaunch(helperInput(t, self, wd, req(), filepath.Join(wd, "report.json")))
	if err != nil {
		t.Fatal(err)
	}
	if !p.Preview || !p.WouldLaunch || p.Planned == nil {
		t.Fatalf("preview = %+v", p)
	}
	if !strings.HasPrefix(p.Planned.State.Home, "$STATE") {
		t.Fatalf("preview HOME %q is not a placeholder", p.Planned.State.Home)
	}
	applied, ps, _ := runContained(t, helperInput(t, self, wd, req(), filepath.Join(wd, "report.json")))
	if !ps.Success() {
		t.Fatalf("helper exited %v", ps)
	}
	if strings.HasPrefix(applied.State.Home, "$") {
		t.Fatalf("applied HOME %q is a placeholder", applied.State.Home)
	}
	if p.Planned.Fingerprint != applied.Fingerprint {
		t.Fatalf("preview fingerprint %s, applied %s", p.Planned.Fingerprint, applied.Fingerprint)
	}
}

// TestGitWorktreeMetadataNeedsExplicitGrant: a linked worktree's Git metadata
// lives outside the worktree and is denied until the caller grants the common
// directory; the primary checkout stays out either way.
func TestGitWorktreeMetadataNeedsExplicitGrant(t *testing.T) {
	requireABI(t)
	self := testHarness(t)
	primary := t.TempDir()
	common := filepath.Join(primary, ".git")
	gitdir := filepath.Join(common, "worktrees", "wt")
	mkdirWith(t, gitdir, "HEAD")
	_ = os.WriteFile(filepath.Join(gitdir, "commondir"), []byte("../..\n"), 0o600)
	_ = os.WriteFile(filepath.Join(common, "config"), []byte("[core]\n"), 0o600)
	_ = os.WriteFile(filepath.Join(primary, "README"), []byte("primary"), 0o600)
	wt := t.TempDir()
	_ = os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+gitdir+"\n"), 0o600)

	run := func(req *containment.Request, want string) {
		t.Helper()
		report := filepath.Join(wt, "report.json")
		cases := []struct{ check, want string }{
			{"read=" + filepath.Join(wt, ".git"), "ok"},
			{"read=" + filepath.Join(gitdir, "HEAD"), want},
			{"read=" + filepath.Join(common, "config"), want},
			{"write=" + filepath.Join(gitdir, "index.lock"), want},
			{"read=" + filepath.Join(primary, "README"), "EACCES"},
		}
		_, ps, _ := runContained(t, helperInput(t, self, wt, req, checkArgs(report, cases)...))
		if !ps.Success() {
			t.Fatalf("helper exited %v", ps)
		}
		checkReport(t, readHelperReport(t, report), cases)
	}
	run(&containment.Request{Kind: containment.KindLandlock}, "EACCES")
	run(&containment.Request{Kind: containment.KindLandlock, ReadWrite: []string{common}}, "ok")
}

// TestSupervisionTeardown leaves descendants of every shape a process-group
// kill misses and checks each one is gone once Finish returns.
func TestSupervisionTeardown(t *testing.T) {
	requireABI(t)
	if _, reason := probeSupervision(); reason != "" {
		if os.Getenv("HW_LANDLOCK_REQUIRE_SUPERVISION") != "" {
			t.Fatalf("cgroup supervision required: %s", reason)
		}
		t.Skipf("cgroup supervision unavailable: %s", reason)
	}
	self := testHarness(t)
	for iter := 0; iter < 5; iter++ {
		wd := t.TempDir()
		marker := fmt.Sprintf("hwc-marker-%d-%d", os.Getpid(), iter)
		ready := filepath.Join(wd, "ready")
		in := helperInput(t, self, wd, &containment.Request{Kind: containment.KindLandlock}, marker, ready, map[bool]string{true: "1", false: "0"}[iter%2 == 1])
		in.Env = append(in.Env[:len(in.Env)-2], helperEnv+"=descendants", "GODEBUG=containermaxprocs=0")
		l, err := Prepare(in)
		if err != nil {
			t.Fatal(err)
		}
		master, slave, _ := OpenPTYPair()
		_ = l.AddTerminal(slave)
		pid, err := l.Start(slave)
		_ = unix.Close(slave)
		if err != nil {
			l.Release()
			t.Fatal(err)
		}
		mf := masterFile(master)
		done := drain(mf)
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(ready); err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		before := markerPIDs(marker)
		if len(before) < 8 {
			t.Fatalf("stand-in harness left only %d descendants", len(before))
		}
		_ = syscall.Kill(-pid, syscall.SIGTERM)
		p, _ := os.FindProcess(pid)
		_, _ = p.Wait()
		cleanup := l.Finish(pid, true, time.Now().Add(300*time.Millisecond))
		if cleanup != cleanupComplete {
			t.Fatalf("cleanup: %s", cleanup)
		}
		if left := markerPIDs(marker); len(left) > 0 {
			t.Fatalf("descendants survived populated 0: %v", left)
		}
		if _, err := os.Stat(l.Applied().Supervision.Cgroup); !os.IsNotExist(err) {
			t.Fatalf("session cgroup not removed: %v", err)
		}
		<-done
		_ = mf.Close()
	}
}

// markerPIDs lists live processes whose argv carries marker (trusted test
// instrumentation only).
func markerPIDs(marker string) []int {
	var out []int
	ents, _ := os.ReadDir("/proc")
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile("/proc/" + e.Name() + "/cmdline")
		if err != nil || !strings.Contains(string(b), marker) {
			continue
		}
		if st, err := os.ReadFile("/proc/" + e.Name() + "/stat"); err == nil {
			if i := strings.LastIndexByte(string(st), ')'); i > 0 && strings.HasPrefix(string(st[i+1:]), " Z") {
				continue // a zombie is not running
			}
		}
		out = append(out, pid)
	}
	slices.Sort(out)
	return out
}

func TestStateReuseRecoversPreviousLaunch(t *testing.T) {
	requireABI(t)
	if _, reason := probeSupervision(); reason != "" {
		t.Skipf("cgroup supervision unavailable: %s", reason)
	}
	self := testHarness(t)
	st, err := NewState(true)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	wd := t.TempDir()
	in := helperInput(t, self, wd, &containment.Request{Kind: containment.KindLandlock})
	in.Env = append(in.Env[:len(in.Env)-2], helperEnv+"=sleep", "GODEBUG=containermaxprocs=0")
	in.State = st
	l, err := Prepare(in)
	if err != nil {
		t.Fatal(err)
	}
	master, slave, _ := OpenPTYPair()
	defer func() { _ = unix.Close(master) }()
	_ = l.AddTerminal(slave)
	pid, err := l.Start(slave)
	_ = unix.Close(slave)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a crashed wrapper: never Finish, drop the lock. A crash ends
	// every thread at once; here the spawn thread may still be exiting, and
	// its private descriptor table keeps the lock held until it has, so the
	// next owner waits for that last reference.
	st.unlock()
	cg := l.Applied().Supervision.Cgroup
	// The next owner recovers the recorded cgroup before reuse.
	st2, err := OpenState(st.ID)
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		err = st2.Remove(t.Context())
		if !errors.Is(err, errStateInUse) || time.Now().After(deadline) {
			break
		}
	}
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	st2.Close()
	if _, err := os.Stat(cg); !os.IsNotExist(err) {
		t.Fatalf("recorded cgroup not recovered: %v", err)
	}
	p, _ := os.FindProcess(pid)
	if ps, _ := p.Wait(); ps == nil || ps.Success() {
		t.Fatalf("the crashed launch's harness was not killed: %v", ps)
	}
}

// TestUnavailableKernelRefuses is the refusal cells' test: on a kernel that
// cannot enforce ABI 9 (below it, Landlock left out of lsm=, or its syscalls
// blocked by seccomp) and has no working AppArmor socket layer, a valid
// contained launch is refused at the kernel stage and starts nothing. Where
// Landlock ABI 9, or ABI 6-8 with the socket layer, is available it is skipped
// — unless HW_LANDLOCK_EXPECT_UNAVAILABLE says this cell exists to prove
// refusal.
func TestUnavailableKernelRefuses(t *testing.T) {
	_, perr := landlockProbe()
	if perr == nil {
		if os.Getenv("HW_LANDLOCK_EXPECT_UNAVAILABLE") != "" {
			t.Fatal("this cell must lack Landlock ABI 9, but it is available")
		}
		t.Skip("Landlock ABI 9 is available here")
	}
	if abi, err := landlock.ABI(); err == nil && abi >= landlock.MinimumABI {
		if _, err := socketLayer(abi); err == nil {
			if os.Getenv("HW_LANDLOCK_EXPECT_UNAVAILABLE") != "" {
				t.Fatal("this cell must refuse, but the AppArmor socket layer is usable")
			}
			t.Skip("the AppArmor socket layer makes this kernel usable")
		}
	}
	self := testHarness(t)
	wd := t.TempDir()
	marker := filepath.Join(wd, "started")
	in := helperInput(t, self, wd, &containment.Request{Kind: containment.KindLandlock}, marker)
	l, err := Prepare(in)
	if err == nil {
		l.Release()
		t.Fatal("a kernel without Landlock ABI 9 accepted a contained launch")
	}
	var re *RefusalError
	if !errors.As(err, &re) || re.Stage != StageKernel {
		t.Fatalf("refusal %v, want stage %q", err, StageKernel)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the harness started")
	}
	t.Logf("refused as designed: %v", err)
}

// TestPreviewReportsGitMetadata: a linked worktree's metadata outside every
// grant is reported, never granted automatically.
func TestPreviewReportsGitMetadata(t *testing.T) {
	self := testHarness(t)
	primary := t.TempDir()
	gitdir := filepath.Join(primary, ".git", "worktrees", "wt")
	if err := os.MkdirAll(gitdir, 0o700); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(gitdir, "commondir"), []byte("../..\n"), 0o600)
	wt := t.TempDir()
	_ = os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+gitdir+"\n"), 0o600)
	p, err := PreviewLaunch(helperInput(t, self, wt, &containment.Request{Kind: containment.KindLandlock}))
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(p.Notes, "\n")
	if !strings.Contains(joined, gitdir) || !strings.Contains(joined, filepath.Join(primary, ".git")) {
		t.Fatalf("notes = %v", p.Notes)
	}
	for _, g := range p.Planned.Grants {
		if strings.HasPrefix(g.Path, primary) {
			t.Fatalf("primary checkout granted automatically: %+v", g)
		}
	}
	// Granting the common directory silences both notes.
	req := &containment.Request{Kind: containment.KindLandlock, ReadWrite: []string{filepath.Join(primary, ".git")}}
	p, err = PreviewLaunch(helperInput(t, self, wt, req))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Notes) != 0 {
		t.Fatalf("notes after granting: %v", p.Notes)
	}
}

// TestAppliedPolicyCarriesNoValues: provisioned variables appear by name
// only; no value — credentials above all — reaches the applied policy.
func TestAppliedPolicyCarriesNoValues(t *testing.T) {
	requireABI(t)
	self := testHarness(t)
	wd := t.TempDir()
	const secret = "sk-test-do-not-leak-0123456789"
	req := &containment.Request{Kind: containment.KindLandlock, PassEnv: []string{"HW_TEST_TOKEN"}}
	in := helperInput(t, self, wd, req, filepath.Join(wd, "r.json"))
	in.Env = append(in.Env, "HW_TEST_TOKEN="+secret)
	applied, _, _ := runContained(t, in)
	b, _ := json.Marshal(applied)
	if strings.Contains(string(b), secret) {
		t.Fatal("a variable's value leaked into the applied policy")
	}
	if !slices.Contains(applied.Env, "HW_TEST_TOKEN") {
		t.Fatalf("env names = %v", applied.Env)
	}
}

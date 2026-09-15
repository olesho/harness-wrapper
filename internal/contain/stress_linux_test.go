//go:build linux

package contain

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/olesho/harness-wrapper/internal/landlock"
	"github.com/olesho/harness-wrapper/internal/testsupport/cfd"
	"golang.org/x/sys/unix"
)

// TestContainedSpawnStress is the concurrent-spawn stress run the plan makes
// a release gate: HW_CONTAIN_STRESS=<runs>x<spawns> (CI: 200x320). Each run
// has 8 contained spawners using raw-descriptor PTY pairs, uncontained
// pty.Start sessions alongside, a GC loop, and Go goroutines and (in cgo
// builds) C threads opening non-close-on-exec descriptors throughout. Where
// the kernel has Landlock ABI 9 every contained spawn also enters a domain;
// on the hosted runners' older kernels the private-table spawn runs without
// one. Any failed spawn, child on a terminal it does not own, inherited
// descriptor, surviving spawn thread or change to the wrapper's descriptor
// table fails the test.
func TestContainedSpawnStress(t *testing.T) {
	spec := os.Getenv("HW_CONTAIN_STRESS")
	if spec == "" {
		t.Skip("set HW_CONTAIN_STRESS=<runs>x<spawns> to run the stress test")
	}
	runsS, spawnsS, ok := strings.Cut(spec, "x")
	runs, err1 := strconv.Atoi(runsS)
	spawns, err2 := strconv.Atoi(spawnsS)
	if !ok || err1 != nil || err2 != nil || runs <= 0 || spawns < 8 {
		t.Fatalf("HW_CONTAIN_STRESS=%q: want <runs>x<spawns>, spawns >= 8", spec)
	}
	self, _ := os.Executable()
	self, _ = filepath.EvalSymlinks(self)
	_, abiErr := landlock.Probe()
	t.Logf("go=%s cgo-openers=%v landlock=%v GOMAXPROCS=%d", runtime.Version(), cfd.Available, abiErr == nil, runtime.GOMAXPROCS(0))
	var total time.Duration
	for run := 0; run < runs; run++ {
		d, failures := stressRun(t, self, spawns, abiErr == nil)
		total += d
		if len(failures) > 0 {
			t.Fatalf("run %d/%d: %d failures, first: %s", run+1, runs, len(failures), strings.Join(failures[:min(5, len(failures))], " | "))
		}
	}
	t.Logf("%d runs x %d spawns: %s per contained spawn", runs, spawns, (total / time.Duration(runs*spawns)).Round(time.Microsecond))
}

// stressRuleset builds the domain every contained spawn of a run enters: the
// test binary and system libraries read/execute, /proc read, /dev/null, and the
// report directory writable.
func stressRuleset(t *testing.T, self, reports string) *landlock.Ruleset {
	t.Helper()
	rs, err := landlock.New(landlock.Config{})
	if err != nil {
		t.Fatal(err)
	}
	grant := func(path string, access landlock.AccessFS) {
		fd, err := unix.Open(path, unix.O_PATH|unix.O_CLOEXEC, 0)
		if err != nil {
			return // optional system path
		}
		defer func() { _ = unix.Close(fd) }()
		var st unix.Stat_t
		_ = unix.Fstat(fd, &st)
		if err := rs.AddPath(landlock.Rule{FD: fd, Access: access, IsDir: st.Mode&unix.S_IFMT == unix.S_IFDIR}); err != nil {
			t.Fatal(err)
		}
	}
	grant(filepath.Dir(self), landlock.ReadExec)
	for _, p := range []string{"/usr", "/lib", "/lib64", "/etc/ld.so.cache"} {
		grant(p, landlock.ReadExec)
	}
	grant("/proc", landlock.ReadOnly)
	grant("/dev/null", landlock.AccessFSReadFile|landlock.AccessFSWriteFile|landlock.AccessFSTruncate)
	grant(reports, landlock.ReadWrite)
	return rs
}

func stressRun(t *testing.T, self string, spawns int, withDomain bool) (time.Duration, []string) {
	t.Helper()
	dir := t.TempDir()
	reports := filepath.Join(dir, "reports")
	_ = os.Mkdir(reports, 0o700)
	secret := filepath.Join(dir, "secret")
	_ = os.WriteFile(secret, []byte("s3cret"), 0o600)
	var rs *landlock.Ruleset
	if withDomain {
		rs = stressRuleset(t, self, reports)
		defer func() { _ = rs.Close() }()
	}
	env := append(os.Environ(), helperEnv+"=report", "GODEBUG=containermaxprocs=0")

	var mu sync.Mutex
	var failures []string
	fail := func(format string, args ...any) {
		mu.Lock()
		failures = append(failures, fmt.Sprintf(format, args...))
		mu.Unlock()
	}

	// Planted and continuously created non-close-on-exec descriptors.
	planted, _ := syscall.Open(secret, syscall.O_RDONLY, 0)
	defer func() { _ = syscall.Close(planted) }()
	if cfd.Available {
		if fd := cfd.OpenNonCloexec(secret); fd >= 0 {
			defer cfd.Close(fd)
		}
	}
	// The wrapper's table before anything transient runs: an uncontained
	// session's fork holds a pipe and a pidfd for a moment.
	before := fdStateQuiet()
	if cfd.Available && cfd.StartOpeners(secret, 4) > 0 {
		defer cfd.StopOpeners()
	}
	var stop atomic.Bool
	var bg sync.WaitGroup
	for i := 0; i < 4; i++ {
		bg.Add(1)
		go func() {
			defer bg.Done()
			for !stop.Load() {
				if fd, err := syscall.Open(secret, syscall.O_RDONLY, 0); err == nil {
					_ = syscall.Close(fd)
				}
			}
		}()
	}
	bg.Add(2)
	go func() {
		defer bg.Done()
		for !stop.Load() {
			runtime.GC()
			time.Sleep(time.Millisecond)
		}
	}()
	// Uncontained sessions alongside, on the old pty.Start path.
	go func() {
		defer bg.Done()
		for i := 0; !stop.Load(); i++ {
			report := filepath.Join(reports, fmt.Sprintf("plain-%d.json", i))
			cmd := exec.Command(self, report)
			cmd.Env = env
			ptmx, err := pty.Start(cmd)
			if err != nil {
				fail("uncontained pty.Start: %v", err)
				return
			}
			done := drain(ptmx)
			if err := cmd.Wait(); err != nil {
				fail("uncontained child: %v", err)
			}
			_ = ptmx.Close()
			<-done
		}
	}()

	var tidMu sync.Mutex
	var tids []int
	const spawners = 8
	per := spawns / spawners
	start := time.Now()
	var wg sync.WaitGroup
	for s := 0; s < spawners; s++ {
		wg.Add(1)
		go func(s int) {
			defer wg.Done()
			for i := 0; i < per; i++ {
				report := filepath.Join(reports, fmt.Sprintf("c-%d-%d.json", s, i))
				master, slave, err := OpenPTYPair()
				if err != nil {
					fail("pty pair: %v", err)
					continue
				}
				want, _ := os.Readlink("/proc/self/fd/" + strconv.Itoa(slave))
				r := spawn(spawnSpec{path: self, argv: []string{self, report}, env: env, dir: dir, tty: slave, cgroupFD: -1, ruleset: rs})
				_ = unix.Close(slave)
				if r.err != nil {
					fail("spawn %s: %v", r.stage, r.err)
					_ = unix.Close(master)
					continue
				}
				tidMu.Lock()
				tids = append(tids, r.tid)
				tidMu.Unlock()
				mf := masterFile(master)
				done := drain(mf)
				p, _ := os.FindProcess(r.pid)
				st, err := p.Wait()
				switch {
				case err != nil:
					fail("wait: %v", err)
				case !st.Success():
					fail("child %s", st)
				}
				select {
				case <-done:
				case <-time.After(10 * time.Second):
					fail("PTY master never reached EOF")
				}
				_ = mf.Close()
				rep, err := readReport(report)
				if err != nil {
					fail("report: %v", err)
					continue
				}
				if rep.FDs["0"] != want {
					fail("child fd 0 is %q, not its own terminal %q", rep.FDs["0"], want)
				}
				if l := leaked(rep); len(l) > 0 {
					fail("child inherited %v", l)
				}
			}
		}(s)
	}
	wg.Wait()
	elapsed := time.Since(start)
	stop.Store(true)
	bg.Wait()

	time.Sleep(100 * time.Millisecond)
	for _, tid := range tids {
		if _, err := os.Stat(fmt.Sprintf("/proc/self/task/%d", tid)); err == nil {
			fail("spawn thread %d survived", tid)
		}
	}
	after := fdStateQuiet()
	for fd, desc := range before {
		if after[fd] != desc {
			fail("wrapper descriptor %d changed: %q -> %q", fd, desc, after[fd])
		}
	}
	return elapsed, failures
}

// readReport is readHelperReport without a *testing.T, for worker goroutines.
func readReport(path string) (helperReport, error) {
	var rep helperReport
	b, err := os.ReadFile(path)
	if err != nil {
		return rep, err
	}
	return rep, json.Unmarshal(b, &rep)
}

// fdStateQuiet snapshots the wrapper's descriptors that exist throughout a
// run: the ones below the first free number when it starts, excluding
// terminals and the transient descriptors the run itself opens and closes.
func fdStateQuiet() map[int]string {
	st := map[int]string{}
	ents, _ := os.ReadDir("/proc/self/fd")
	for _, e := range ents {
		n, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		target, _ := os.Readlink("/proc/self/fd/" + e.Name())
		if target == "" || strings.HasPrefix(target, "/dev/pts/") || target == "/dev/ptmx" ||
			strings.Contains(target, "secret") || strings.Contains(target, "reports") {
			continue
		}
		fl, err := unix.FcntlInt(uintptr(n), unix.F_GETFD, 0)
		if err != nil {
			continue
		}
		st[n] = fmt.Sprintf("%s cloexec=%v", target, fl&unix.FD_CLOEXEC != 0)
	}
	return st
}

//go:build linux

package contain

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// The test binary doubles as the contained child: TestMain dispatches on
// HW_CONTAIN_HELPER before the testing framework (or anything else that opens
// a file) runs, so the descriptor listing sees only what the child inherited.
// Children run with GODEBUG=containermaxprocs=0 so the runtime does not keep
// cgroup cpu.max open either.
const helperEnv = "HW_CONTAIN_HELPER"

// init pins the m0 helper to the main thread: LockOSThread in an init
// function makes main (and so TestMain) run on the main thread.
func init() {
	if os.Getenv(helperEnv) == "m0" {
		runtime.LockOSThread()
	}
}

func TestMain(m *testing.M) {
	if mode := os.Getenv(helperEnv); mode != "" {
		os.Exit(runHelper(mode, os.Args[1:]))
	}
	os.Exit(m.Run())
}

type helperReport struct {
	PID    int               `json:"pid"`
	FDs    map[string]string `json:"fds"`
	Checks map[string]string `json:"checks,omitempty"`
	Env    []string          `json:"env,omitempty"`
	Cwd    string            `json:"cwd,omitempty"`
}

func runHelper(mode string, args []string) int {
	switch mode {
	case "report":
		return helperReportMode(args)
	case "descendants":
		return helperDescendants(args)
	case "child":
		return helperChild(args)
	case "sleep":
		for {
			time.Sleep(time.Hour)
		}
	case "m0":
		return helperM0(args)
	}
	fmt.Fprintln(os.Stderr, "unknown helper mode", mode)
	return 2
}

// rawFDs lists this process's descriptors with raw syscalls, before anything
// initialises the netpoller.
func rawFDs() map[string]string {
	out := map[string]string{}
	dfd, err := syscall.Open("/proc/self/fd", syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		out["error"] = err.Error()
		return out
	}
	defer func() { _ = syscall.Close(dfd) }()
	self := strconv.Itoa(dfd)
	buf := make([]byte, 8192)
	for {
		n, err := syscall.ReadDirent(dfd, buf)
		if err != nil || n <= 0 {
			break
		}
		var names []string
		_, _, names = syscall.ParseDirent(buf[:n], -1, names)
		for _, name := range names {
			if name == self {
				continue
			}
			t, _ := os.Readlink("/proc/self/fd/" + name)
			out[name] = t
		}
	}
	return out
}

func errName(err error) string {
	if err == nil {
		return "ok"
	}
	var en syscall.Errno
	if errors.As(err, &en) {
		return unix.ErrnoName(en)
	}
	return err.Error()
}

func helperReportMode(args []string) int {
	rep := helperReport{PID: os.Getpid(), FDs: rawFDs(), Checks: map[string]string{}}
	report := args[0]
	for _, c := range args[1:] {
		k, v, _ := strings.Cut(c, "=")
		if strings.HasPrefix(v, "$") {
			v = os.ExpandEnv(v) // $HOME/…: private paths only the child knows
		}
		switch k {
		case "read":
			_, err := os.ReadFile(v)
			rep.Checks[c] = errName(err)
		case "write":
			rep.Checks[c] = errName(os.WriteFile(v, []byte("x"), 0o600))
		case "truncate":
			rep.Checks[c] = errName(os.Truncate(v, 0))
		case "remove":
			rep.Checks[c] = errName(os.Remove(v))
		case "rename":
			from, to, _ := strings.Cut(v, ":")
			rep.Checks[c] = errName(os.Rename(from, to))
		case "cgroup-move", "cgroup-mkdir":
			rep.Checks[c] = errName(leaveCgroup(k == "cgroup-mkdir"))
		case "mkdir":
			rep.Checks[c] = errName(os.Mkdir(v, 0o700))
		case "exec":
			rep.Checks[c] = errName(exec.Command(v, "--version").Run())
		case "connect-unix":
			fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
			if err == nil {
				err = unix.Connect(fd, &unix.SockaddrUnix{Name: v})
				_ = unix.Close(fd)
			}
			rep.Checks[c] = errName(err)
		case "connect-abstract":
			fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
			if err == nil {
				err = unix.Connect(fd, &unix.SockaddrUnix{Name: "\x00" + v})
				_ = unix.Close(fd)
			}
			rep.Checks[c] = errName(err)
		case "listen-unix":
			// A server created inside the domain must stay reachable from it.
			ln, err := net.Listen("unix", v)
			if err == nil {
				go func() {
					for {
						cn, err := ln.Accept()
						if err != nil {
							return
						}
						_ = cn.Close()
					}
				}()
				cn, derr := net.Dial("unix", v)
				if derr == nil {
					_ = cn.Close()
				}
				err = derr
				_ = ln.Close()
			}
			rep.Checks[c] = errName(err)
		case "kill":
			pid, _ := strconv.Atoi(v)
			rep.Checks[c] = errName(syscall.Kill(pid, 0))
		case "tcp":
			port, _ := strconv.Atoi(v)
			fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
			if err == nil {
				err = unix.Connect(fd, &unix.SockaddrInet4{Port: port, Addr: [4]byte{127, 0, 0, 1}})
				_ = unix.Close(fd)
			}
			rep.Checks[c] = errName(err)
		case "bind":
			fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
			if err == nil {
				err = unix.Bind(fd, &unix.SockaddrInet4{Port: 0, Addr: [4]byte{127, 0, 0, 1}})
				_ = unix.Close(fd)
			}
			rep.Checks[c] = errName(err)
		case "env":
			rep.Env = os.Environ()
		case "cwd":
			rep.Cwd, _ = os.Getwd()
		case "sleep":
			ms, _ := strconv.Atoi(v)
			time.Sleep(time.Duration(ms) * time.Millisecond)
		}
	}
	b, _ := json.Marshal(rep)
	if err := os.WriteFile(report, b, 0o600); err != nil {
		fmt.Fprintln(os.Stderr, "write report:", err)
		return 3
	}
	return 0
}

// leaveCgroup tries to escape the cgroup this process runs in: by moving
// itself into the parent cgroup, which the delegation rules alone would allow,
// or by creating a sub-cgroup to move into.
func leaveCgroup(mkdir bool) error {
	b, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return err
	}
	rel, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "0::")
	if !ok {
		return fmt.Errorf("not on a unified cgroup hierarchy: %q", b)
	}
	own := filepath.Join("/sys/fs/cgroup", rel)
	if mkdir {
		return os.Mkdir(filepath.Join(own, "escape"), 0o755)
	}
	f, err := os.OpenFile(filepath.Join(filepath.Dir(own), "cgroup.procs"), os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	_, err = f.WriteString(strconv.Itoa(os.Getpid()))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// helperDescendants is a stand-in harness that leaves behind every
// descendant shape a process-group kill misses, then waits to be killed:
// TERM-ignoring children, setsid children, double-forked orphans, a child
// that starts a detached process on SIGTERM, and (optionally) a fork loop.
//
//	descendants <marker> <readyfile> <forkloop 0|1>
func helperDescendants(args []string) int {
	marker, ready, forkloop := args[0], args[1], args[2] == "1"
	self, _ := os.Executable()
	start := func(role string) {
		c := exec.Command(self, role, marker)
		c.Env = append(os.Environ(), helperEnv+"=child")
		if err := c.Start(); err == nil {
			go func() { _ = c.Wait() }()
		}
	}
	for _, role := range []string{"ignoreterm", "ignoreterm", "setsid", "setsid", "doublefork", "doublefork", "termforker"} {
		start(role)
	}
	if forkloop {
		start("forkloop")
	}
	time.Sleep(200 * time.Millisecond)
	_ = os.WriteFile(ready, []byte("1"), 0o600)
	for {
		time.Sleep(time.Hour)
	}
}

func helperChild(args []string) int {
	role, marker := args[0], args[1]
	self, _ := os.Executable()
	spawn := func(r string) {
		c := exec.Command(self, r, marker)
		c.Env = os.Environ()
		if err := c.Start(); err == nil {
			go func() { _ = c.Wait() }()
		}
	}
	switch role {
	case "ignoreterm":
		signal.Ignore(syscall.SIGTERM)
	case "setsid", "orphan":
		_, _ = unix.Setsid()
		signal.Ignore(syscall.SIGTERM)
	case "doublefork":
		spawn("orphan")
		time.Sleep(20 * time.Millisecond)
		return 0
	case "termforker":
		ch := make(chan os.Signal, 4)
		signal.Notify(ch, syscall.SIGTERM)
		for range ch {
			spawn("orphan")
		}
	case "forkloop":
		signal.Ignore(syscall.SIGTERM)
		for i := 0; i < 5000; i++ {
			spawn("brief")
			time.Sleep(time.Millisecond)
		}
	case "brief":
		time.Sleep(time.Duration(os.Getpid()%5) * time.Millisecond)
		return 0
	}
	for {
		time.Sleep(time.Hour)
	}
}

// helperM0 forces the spawn onto the main thread. The process starts with
// GOMAXPROCS=1 and main pinned to the main thread (see init); main unlocks and
// spawns at once, so the spawn goroutine is the next to run on this very
// thread when main blocks — the case the hand-off exists for. The runtime may
// still schedule it elsewhere, so a few attempts are made; the helper fails
// unless one handed off and every PTY master reached EOF (a private table left
// on a parked main thread would hold the slave open forever).
func helperM0(args []string) int {
	if unix.Gettid() != unix.Getpid() {
		fmt.Println("helper is not on the main thread")
		return 1
	}
	runtime.UnlockOSThread()
	self, _ := os.Executable()
	env := append(os.Environ(), helperEnv+"=report", "GODEBUG=containermaxprocs=0")
	handedOff := false
	for attempt := 0; attempt < 8 && !handedOff; attempt++ {
		master, slave, err := OpenPTYPair()
		if err != nil {
			fmt.Println("pty:", err)
			return 1
		}
		r := spawn(spawnSpec{
			path: self, argv: []string{self, filepath.Join(args[0], fmt.Sprintf("m0-%d.json", attempt)), "sleep=50"},
			env: env, dir: args[0], tty: slave, cgroupFD: -1,
		})
		_ = unix.Close(slave)
		if r.err != nil {
			fmt.Println("spawn:", r.stage, r.err)
			return 1
		}
		handedOff = r.handedOff
		p, _ := os.FindProcess(r.pid)
		_, _ = p.Wait()
		mf := masterFile(master)
		select {
		case <-drain(mf):
		case <-time.After(5 * time.Second):
			fmt.Printf("attempt %d: PTY master never reached EOF (handedOff=%v)\n", attempt, r.handedOff)
			return 1
		}
		_ = mf.Close()
	}
	fmt.Printf("handedOff=%v eof=true\n", handedOff)
	if !handedOff {
		return 1
	}
	return 0
}

// --- parent-side helpers ---

func readHelperReport(t *testing.T, path string) helperReport {
	t.Helper()
	var rep helperReport
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read child report: %v", err)
	}
	if err := json.Unmarshal(b, &rep); err != nil {
		t.Fatalf("parse child report: %v", err)
	}
	return rep
}

// leaked returns the child's descriptors other than 0, 1 and 2.
func leaked(rep helperReport) []string {
	var out []string
	for k, v := range rep.FDs {
		if k != "0" && k != "1" && k != "2" {
			out = append(out, k+"->"+v)
		}
	}
	sort.Strings(out)
	return out
}

// fdState snapshots the calling thread's descriptor table with flags.
func fdState(t *testing.T) map[int]string {
	t.Helper()
	st := map[int]string{}
	ents, err := os.ReadDir("/proc/thread-self/fd")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		n, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		fl, err := unix.FcntlInt(uintptr(n), unix.F_GETFD, 0)
		if err != nil {
			continue // the ReadDir descriptor, already closed
		}
		target, _ := os.Readlink("/proc/thread-self/fd/" + e.Name())
		if strings.HasPrefix(target, "/dev/pts/") || target == "/dev/ptmx" {
			continue
		}
		st[n] = fmt.Sprintf("%s cloexec=%v", target, fl&unix.FD_CLOEXEC != 0)
	}
	return st
}

func requireABI(t *testing.T) int {
	t.Helper()
	abi, err := landlockProbe()
	if err != nil {
		if os.Getenv("HW_LANDLOCK_REQUIRE_ABI") != "" && os.Getenv("HW_LANDLOCK_REQUIRE_ABI") != "0" {
			t.Fatalf("Landlock ABI %d required by this cell: %v", 9, err)
		}
		t.Skipf("Landlock ABI 9 unavailable: %v", err)
	}
	return abi
}

func waitPID(t *testing.T, pid int) *os.ProcessState {
	t.Helper()
	p, err := os.FindProcess(pid)
	if err != nil {
		t.Fatal(err)
	}
	st, err := p.Wait()
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// drain reads a PTY master until it reports the end (EIO once no process holds
// the slave), so a chatty child never blocks on a full terminal buffer.
//
// It reads through an *os.File, never a raw descriptor number. Closing the
// file while a read is in flight defers the close until the read returns, so
// the number cannot be reused underneath the reader. A raw-number reader whose
// owner closed the number could land on another session's slave, and two
// blocked reads then held each other's PTY side open forever.
func drain(master *os.File) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 4096)
		for {
			if _, err := master.Read(buf); err != nil {
				return
			}
		}
	}()
	return done
}

// masterFile wraps a raw PTY master from OpenPTYPair the way pkg/wrapper
// does: a blocking descriptor os.NewFile keeps out of the netpoller.
func masterFile(fd int) *os.File { return os.NewFile(uintptr(fd), "/dev/ptmx") }

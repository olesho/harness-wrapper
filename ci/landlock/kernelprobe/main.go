//go:build linux

// Command kernelprobe records the kernel, LSM, seccomp and cgroup facts a
// Landlock security job depends on, and exits non-zero when the environment
// cannot enforce the required Landlock ABI. It never skips: a missing
// capability is a failure unless -expect-unavailable says the cell exists to
// prove refusal.
//
//	kernelprobe -min-abi 9 -json probe.json            # required cells
//	kernelprobe -min-abi 9 -expect-unavailable ...     # below-min / disabled / seccomp cells
//	kernelprobe -seccomp-deny-landlock -- CMD ...      # fault injection: Landlock syscalls return ENOSYS
package main

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// ABI 9 value that golang.org/x/sys v0.43.0 does not define.
const accessFSResolveUnix = 1 << 16

// Every filesystem right up to and including ABI 9: bits 0..15 plus RESOLVE_UNIX.
const allFSRightsABI9 = (accessFSResolveUnix << 1) - 1

type check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

type report struct {
	Time        string            `json:"time"`
	Kernel      map[string]string `json:"kernel"`
	Cmdline     string            `json:"cmdline"`
	LSM         string            `json:"lsm"`
	KConfigFrom string            `json:"kconfig_source"`
	KConfig     map[string]string `json:"kconfig"`
	ABI         int               `json:"landlock_abi"`
	ABIErr      string            `json:"landlock_abi_error,omitempty"`
	Errata      int               `json:"landlock_errata"`
	Seccomp     map[string]string `json:"seccomp"`
	Cgroup      map[string]string `json:"cgroup"`
	Go          map[string]string `json:"go"`
	Checks      []check           `json:"checks"`
	MinABI      int               `json:"min_abi"`
	Mode        string            `json:"mode"`
	Verdict     string            `json:"verdict"`
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "-child-smoke" {
		os.Exit(childSmoke(os.Args[2:]))
	}
	if len(os.Args) > 2 && os.Args[1] == "-seccomp-deny-landlock" && os.Args[2] == "--" {
		os.Exit(seccompDenyLandlockExec(os.Args[3:]))
	}
	minABI := flag.Int("min-abi", 9, "minimum Landlock ABI the job requires")
	kconfig := flag.String("config", "", "kernel config file (default: /proc/config.gz, then /boot/config-$(uname -r))")
	jsonOut := flag.String("json", "", "write the report as JSON to this path")
	expectUnavailable := flag.Bool("expect-unavailable", false, "succeed only if Landlock at -min-abi is NOT enforceable (refusal cells)")
	flag.Parse()

	r := &report{Time: time.Now().UTC().Format(time.RFC3339), MinABI: *minABI, Mode: "require"}
	if *expectUnavailable {
		r.Mode = "expect-unavailable"
	}
	var u unix.Utsname
	_ = unix.Uname(&u)
	r.Kernel = map[string]string{
		"release": unix.ByteSliceToString(u.Release[:]),
		"version": unix.ByteSliceToString(u.Version[:]),
		"machine": unix.ByteSliceToString(u.Machine[:]),
	}
	r.Cmdline = strings.TrimSpace(readFile("/proc/cmdline"))
	r.LSM = strings.TrimSpace(readFile("/sys/kernel/security/lsm"))
	r.KConfigFrom, r.KConfig = readKConfig(*kconfig, r.Kernel["release"], []string{
		"CONFIG_SECURITY_LANDLOCK", "CONFIG_LSM", "CONFIG_AUDIT", "CONFIG_SECCOMP_FILTER",
		"CONFIG_CGROUPS", "CONFIG_PROC_CHILDREN", "CONFIG_USER_NS",
	})
	r.Seccomp = procStatus("/proc/self/status", "Seccomp", "Seccomp_filters", "NoNewPrivs")
	r.Cgroup = cgroupFacts()
	r.Go = goFacts()

	abi, err := landlockABI()
	r.ABI = abi
	if err != nil {
		r.ABIErr = err.Error()
	}
	if errata, err := landlockCreate(nil, 0, unix.LANDLOCK_CREATE_RULESET_ERRATA); err == nil {
		r.Errata = errata
	}

	add := func(name string, ok bool, format string, a ...any) {
		r.Checks = append(r.Checks, check{Name: name, OK: ok, Detail: fmt.Sprintf(format, a...)})
	}
	add("landlock_abi", err == nil && abi >= *minABI, "abi=%d required>=%d err=%v", abi, *minABI, err)
	if r.LSM != "" {
		add("lsm_lists_landlock", hasWord(r.LSM, "landlock"), "lsm=%q", r.LSM)
	}
	if err == nil && abi >= *minABI {
		fd, cerr := createABI9Ruleset()
		add("abi9_ruleset", cerr == nil, "handled fs=%#x net=bind|connect scoped=abstract|signal err=%v", allFSRightsABI9, cerr)
		if cerr == nil {
			_ = unix.Close(fd)
			ok, detail := runSmoke()
			add("enforcement_smoke", ok, "%s", detail)
		}
	}

	pass := true
	for _, c := range r.Checks {
		pass = pass && c.OK
	}
	switch {
	case !*expectUnavailable && pass:
		r.Verdict = "PASS: Landlock ABI >= required is enforceable here"
	case !*expectUnavailable:
		r.Verdict = "FAIL: required Landlock capability missing — this job fails, it does not skip"
	case pass:
		r.Verdict = "FAIL: this cell must lack the capability but Landlock is fully enforceable"
	default:
		r.Verdict = "PASS: capability unavailable, as this refusal cell requires"
	}

	out, _ := json.MarshalIndent(r, "", "  ")
	if *jsonOut != "" {
		if err := os.WriteFile(*jsonOut, append(out, '\n'), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "write json:", err)
			os.Exit(2)
		}
	}
	printSummary(r)
	if strings.HasPrefix(r.Verdict, "FAIL") {
		os.Exit(1)
	}
}

func printSummary(r *report) {
	fmt.Printf("kernel     %s %s\n", r.Kernel["release"], r.Kernel["machine"])
	fmt.Printf("cmdline    %s\n", r.Cmdline)
	fmt.Printf("lsm        %s\n", orNone(r.LSM))
	fmt.Printf("kconfig    (%s)", r.KConfigFrom)
	for _, k := range []string{"CONFIG_SECURITY_LANDLOCK", "CONFIG_LSM", "CONFIG_AUDIT", "CONFIG_SECCOMP_FILTER"} {
		fmt.Printf(" %s=%s", k, orNone(r.KConfig[k]))
	}
	fmt.Println()
	fmt.Printf("landlock   abi=%d errata=%#x %s\n", r.ABI, r.Errata, r.ABIErr)
	fmt.Printf("seccomp    Seccomp=%s filters=%s NoNewPrivs=%s\n", r.Seccomp["Seccomp"], r.Seccomp["Seccomp_filters"], r.Seccomp["NoNewPrivs"])
	fmt.Printf("cgroup     %s\n", r.Cgroup["summary"])
	fmt.Printf("go         %s %s/%s cgo=%s\n", r.Go["version"], r.Go["goos"], r.Go["goarch"], r.Go["cgo"])
	for _, c := range r.Checks {
		mark := "ok  "
		if !c.OK {
			mark = "FAIL"
		}
		fmt.Printf("  [%s] %-18s %s\n", mark, c.Name, c.Detail)
	}
	fmt.Println(r.Verdict)
}

func landlockCreate(attr unsafe.Pointer, size uintptr, flags uintptr) (int, error) {
	r, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, uintptr(attr), size, flags)
	if errno != 0 {
		return -1, errno
	}
	return int(r), nil
}

func landlockABI() (int, error) {
	abi, err := landlockCreate(nil, 0, unix.LANDLOCK_CREATE_RULESET_VERSION)
	switch {
	case errors.Is(err, unix.EOPNOTSUPP):
		return 0, fmt.Errorf("landlock built in but disabled at boot (lsm=): %w", err)
	case errors.Is(err, unix.ENOSYS):
		return 0, fmt.Errorf("landlock syscalls unavailable (not built, or blocked by seccomp): %w", err)
	case err != nil:
		return 0, err
	}
	return abi, nil
}

func createABI9Ruleset() (int, error) {
	attr := unix.LandlockRulesetAttr{
		Access_fs:  allFSRightsABI9,
		Access_net: unix.LANDLOCK_ACCESS_NET_BIND_TCP | unix.LANDLOCK_ACCESS_NET_CONNECT_TCP,
		Scoped:     unix.LANDLOCK_SCOPE_ABSTRACT_UNIX_SOCKET | unix.LANDLOCK_SCOPE_SIGNAL,
	}
	return landlockCreate(unsafe.Pointer(&attr), unsafe.Sizeof(attr), 0)
}

// runSmoke starts a copy of this binary that restricts one locked thread and
// tries operations outside the domain; the parent owns the targets.
func runSmoke() (bool, string) {
	dir, err := os.MkdirTemp("", "kprobe-")
	if err != nil {
		return false, "mkdtemp: " + err.Error()
	}
	defer func() { _ = os.RemoveAll(dir) }()
	granted := filepath.Join(dir, "granted")
	denied := filepath.Join(dir, "denied")
	for _, d := range []string{granted, denied} {
		if err := os.Mkdir(d, 0o700); err != nil {
			return false, err.Error()
		}
		if err := os.WriteFile(filepath.Join(d, "f"), []byte("x"), 0o600); err != nil {
			return false, err.Error()
		}
	}
	sockPath := filepath.Join(denied, "s.sock")
	pathLn, err := net.Listen("unix", sockPath)
	if err != nil {
		return false, "listen unix: " + err.Error()
	}
	defer func() { _ = pathLn.Close() }()
	abstract := fmt.Sprintf("@kprobe-%d", os.Getpid())
	absLn, err := net.Listen("unix", abstract)
	if err != nil {
		return false, "listen abstract: " + err.Error()
	}
	defer func() { _ = absLn.Close() }()
	tcpLn, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return false, "listen tcp: " + err.Error()
	}
	defer func() { _ = tcpLn.Close() }()
	port := tcpLn.Addr().(*net.TCPAddr).Port
	usr1 := make(chan os.Signal, 1)
	signal.Notify(usr1, syscall.SIGUSR1)
	defer signal.Stop(usr1)

	cmd := exec.Command("/proc/self/exe", "-child-smoke", granted, denied, sockPath, abstract, fmt.Sprint(port), fmt.Sprint(os.Getpid()))
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		return false, fmt.Sprintf("child: %v: %s", err, text)
	}
	select {
	case <-usr1:
		return false, "parent received SIGUSR1 from inside the domain: " + text
	case <-time.After(100 * time.Millisecond):
	}
	return true, text
}

// childSmoke restricts the calling thread only (no TSYNC), then checks that
// each ABI 1..9 control denies what it must.
func childSmoke(args []string) int {
	granted, denied, sockPath, abstract, port, ppid := args[0], args[1], args[2], args[3], args[4], args[5]
	runtime.LockOSThread() // never unlocked; the process exits after the checks

	fd, err := createABI9Ruleset()
	if err != nil {
		fmt.Println("create:", err)
		return 1
	}
	dfd, err := unix.Open(granted, unix.O_PATH|unix.O_CLOEXEC, 0)
	if err != nil {
		fmt.Println("open granted:", err)
		return 1
	}
	rule := unix.LandlockPathBeneathAttr{Allowed_access: unix.LANDLOCK_ACCESS_FS_READ_FILE | unix.LANDLOCK_ACCESS_FS_READ_DIR, Parent_fd: int32(dfd)}
	if _, _, e := unix.Syscall6(unix.SYS_LANDLOCK_ADD_RULE, uintptr(fd), unix.LANDLOCK_RULE_PATH_BENEATH, uintptr(unsafe.Pointer(&rule)), 0, 0, 0); e != 0 {
		fmt.Println("add_rule:", e)
		return 1
	}
	_ = unix.Close(dfd)
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		fmt.Println("no_new_privs:", err)
		return 1
	}
	// Read before restricting: /proc is outside the domain afterwards.
	st := procStatus("/proc/thread-self/status", "Seccomp", "NoNewPrivs")
	if _, _, e := unix.Syscall(unix.SYS_LANDLOCK_RESTRICT_SELF, uintptr(fd), 0, 0); e != 0 {
		fmt.Println("restrict_self:", e)
		return 1
	}
	_ = unix.Close(fd)

	var fails []string
	var got []string
	expect := func(name string, err error, want unix.Errno) {
		switch {
		case want == 0 && err == nil, want != 0 && errors.Is(err, want):
			got = append(got, name)
		default:
			fails = append(fails, fmt.Sprintf("%s: got %v want %v", name, err, errName(want)))
		}
	}
	_, err = unix.Open(filepath.Join(granted, "f"), unix.O_RDONLY|unix.O_CLOEXEC, 0)
	expect("read-granted", err, 0)
	_, err = unix.Open(filepath.Join(denied, "f"), unix.O_RDONLY|unix.O_CLOEXEC, 0)
	expect("read-denied", err, unix.EACCES)
	_, err = unix.Open(filepath.Join(granted, "f"), unix.O_WRONLY|unix.O_CLOEXEC, 0)
	expect("write-readonly", err, unix.EACCES)
	expect("resolve-unix(abi9)", connectUnix(sockPath), unix.EACCES)
	expect("scope-abstract(abi6)", connectUnix(abstract), unix.EPERM)
	expect("tcp-connect(abi4)", connectTCP(port), unix.EACCES)
	var pid int
	_, _ = fmt.Sscan(ppid, &pid)
	expect("scope-signal(abi6)", unix.Kill(pid, unix.SIGUSR1), unix.EPERM)
	_, err = unix.Open("/proc/self/status", unix.O_RDONLY|unix.O_CLOEXEC, 0)
	expect("proc-outside-domain", err, unix.EACCES)
	if len(fails) > 0 {
		fmt.Printf("denials wrong: %s (ok: %s)\n", strings.Join(fails, "; "), strings.Join(got, ","))
		return 1
	}
	fmt.Printf("thread-scoped domain enforced: %s; child Seccomp=%s NoNewPrivs=%s\n", strings.Join(got, ","), st["Seccomp"], st["NoNewPrivs"])
	return 0
}

func connectUnix(addr string) error {
	s, err := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(s) }()
	name := addr
	if strings.HasPrefix(addr, "@") {
		name = "\x00" + addr[1:]
	}
	return unix.Connect(s, &unix.SockaddrUnix{Name: name})
}

func connectTCP(port string) error {
	var p int
	_, _ = fmt.Sscan(port, &p)
	s, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(s) }()
	return unix.Connect(s, &unix.SockaddrInet4{Port: p, Addr: [4]byte{127, 0, 0, 1}})
}

func errName(e unix.Errno) string {
	if e == 0 {
		return "success"
	}
	return unix.ErrnoName(e)
}

func readFile(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return string(b)
}

func readKConfig(explicit, release string, keys []string) (string, map[string]string) {
	want := map[string]bool{}
	for _, k := range keys {
		want[k] = true
	}
	try := []string{explicit, "/proc/config.gz", "/boot/config-" + release, "/lib/modules/" + release + "/config", "/lib/modules/" + release + "/build/.config"}
	for _, p := range try {
		if p == "" {
			continue
		}
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		var rd io.Reader = f
		if strings.HasSuffix(p, ".gz") {
			if gz, err := gzip.NewReader(f); err == nil {
				rd = gz
			}
		}
		got := map[string]string{}
		sc := bufio.NewScanner(rd)
		for sc.Scan() {
			line := sc.Text()
			if k, v, ok := strings.Cut(line, "="); ok && want[k] {
				got[k] = v
			} else if strings.HasPrefix(line, "# CONFIG_") && strings.HasSuffix(line, " is not set") {
				k := strings.TrimSuffix(strings.TrimPrefix(line, "# "), " is not set")
				if want[k] {
					got[k] = "n"
				}
			}
		}
		_ = f.Close()
		return p, got
	}
	return "unavailable", map[string]string{}
}

func procStatus(path string, keys ...string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(readFile(path), "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		for _, want := range keys {
			if k == want {
				out[k] = strings.TrimSpace(v)
			}
		}
	}
	return out
}

func cgroupFacts() map[string]string {
	var st unix.Statfs_t
	fsType := "unknown"
	if err := unix.Statfs("/sys/fs/cgroup", &st); err == nil {
		if st.Type == unix.CGROUP2_SUPER_MAGIC {
			fsType = "cgroup2"
		} else {
			fsType = fmt.Sprintf("%#x", st.Type)
		}
	}
	self := strings.TrimSpace(readFile("/proc/self/cgroup"))
	ctrl := strings.TrimSpace(readFile("/sys/fs/cgroup/cgroup.controllers"))
	return map[string]string{
		"fs":          fsType,
		"self":        self,
		"controllers": ctrl,
		"summary":     fmt.Sprintf("fs=%s self=%q controllers=%q", fsType, self, ctrl),
	}
}

func goFacts() map[string]string {
	m := map[string]string{"version": runtime.Version(), "goos": runtime.GOOS, "goarch": runtime.GOARCH, "cgo": "unknown"}
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			if s.Key == "CGO_ENABLED" {
				m["cgo"] = s.Value
			}
		}
	}
	return m
}

func hasWord(list, w string) bool {
	for _, f := range strings.Split(list, ",") {
		if strings.TrimSpace(f) == w {
			return true
		}
	}
	return false
}

func orNone(s string) string {
	if s == "" {
		return "<unavailable>"
	}
	return s
}

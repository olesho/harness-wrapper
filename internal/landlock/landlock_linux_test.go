//go:build linux

package landlock

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

func TestNetPortAttrLayout(t *testing.T) {
	if ruleNetPort != 2 {
		t.Fatal("LANDLOCK_RULE_NET_PORT is 2")
	}
	if unsafe.Sizeof(netPortAttr{}) != 16 {
		t.Fatal("struct landlock_net_port_attr is two __u64")
	}
}

func requireABI(t *testing.T) {
	t.Helper()
	if _, err := Probe(0); err != nil {
		if v := os.Getenv("HW_LANDLOCK_REQUIRE_ABI"); v != "" && v != "0" {
			t.Fatalf("Landlock ABI %d required: %v", ResolveUnixABI, err)
		}
		t.Skipf("Landlock unavailable: %v", err)
	}
}

// TestRestrictCurrentThreadOnly restricts one locked thread and checks the
// domain applies there and nowhere else in the process.
func TestRestrictCurrentThreadOnly(t *testing.T) {
	requireABI(t)
	dir := t.TempDir()
	granted := filepath.Join(dir, "granted")
	_ = os.Mkdir(granted, 0o700)
	secret := filepath.Join(dir, "secret")
	_ = os.WriteFile(secret, []byte("x"), 0o600)
	_ = os.WriteFile(filepath.Join(granted, "ok"), []byte("x"), 0o600)

	rs, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rs.Close() }()
	fd, err := unix.Open(granted, unix.O_PATH|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Close(fd) }()
	if err := rs.AddPath(Rule{FD: fd, Access: ReadOnly, IsDir: true}); err != nil {
		t.Fatal(err)
	}
	if err := rs.AddPath(Rule{FD: fd, Access: 0, IsDir: true}); err == nil {
		t.Fatal("an empty rule was accepted")
	}
	if err := rs.AddConnectTCP(443); err == nil {
		t.Fatal("a TCP rule on a ruleset that does not restrict TCP was accepted")
	}

	type result struct{ granted, secret error }
	ch := make(chan result, 1)
	go func() {
		runtime.LockOSThread() // never unlocked: the thread dies with the domain
		if err := rs.RestrictCurrentThread(); err != nil {
			ch <- result{granted: err, secret: err}
			return
		}
		_, g := unix.Open(filepath.Join(granted, "ok"), unix.O_RDONLY|unix.O_CLOEXEC, 0)
		_, s := unix.Open(secret, unix.O_RDONLY|unix.O_CLOEXEC, 0)
		ch <- result{g, s}
	}()
	r := <-ch
	if r.granted != nil || !errors.Is(r.secret, unix.EACCES) {
		t.Fatalf("restricted thread: granted=%v secret=%v", r.granted, r.secret)
	}
	if _, err := os.ReadFile(secret); err != nil {
		t.Fatalf("another thread lost access: %v", err)
	}
}

func TestMinABIAboveKernelIsUnavailable(t *testing.T) {
	requireABI(t)
	abi, _ := ABI()
	if _, err := New(Config{MinABI: abi + 1}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v", err)
	}
}

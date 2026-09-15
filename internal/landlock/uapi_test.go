package landlock

import (
	"slices"
	"testing"
)

// TestUAPIValues pins every Landlock value this package uses to the kernel's
// include/uapi/linux/landlock.h (checked against the v7.1 and v7.2 headers):
// UAPI numbers never change once released, so any difference here is a typo.
func TestUAPIValues(t *testing.T) {
	fs := map[string]AccessFS{
		"LANDLOCK_ACCESS_FS_EXECUTE":      1 << 0,
		"LANDLOCK_ACCESS_FS_WRITE_FILE":   1 << 1,
		"LANDLOCK_ACCESS_FS_READ_FILE":    1 << 2,
		"LANDLOCK_ACCESS_FS_READ_DIR":     1 << 3,
		"LANDLOCK_ACCESS_FS_REMOVE_DIR":   1 << 4,
		"LANDLOCK_ACCESS_FS_REMOVE_FILE":  1 << 5,
		"LANDLOCK_ACCESS_FS_MAKE_CHAR":    1 << 6,
		"LANDLOCK_ACCESS_FS_MAKE_DIR":     1 << 7,
		"LANDLOCK_ACCESS_FS_MAKE_REG":     1 << 8,
		"LANDLOCK_ACCESS_FS_MAKE_SOCK":    1 << 9,
		"LANDLOCK_ACCESS_FS_MAKE_FIFO":    1 << 10,
		"LANDLOCK_ACCESS_FS_MAKE_BLOCK":   1 << 11,
		"LANDLOCK_ACCESS_FS_MAKE_SYM":     1 << 12,
		"LANDLOCK_ACCESS_FS_REFER":        1 << 13,
		"LANDLOCK_ACCESS_FS_TRUNCATE":     1 << 14,
		"LANDLOCK_ACCESS_FS_IOCTL_DEV":    1 << 15,
		"LANDLOCK_ACCESS_FS_RESOLVE_UNIX": 1 << 16,
	}
	got := map[string]AccessFS{
		"LANDLOCK_ACCESS_FS_EXECUTE": AccessFSExecute, "LANDLOCK_ACCESS_FS_WRITE_FILE": AccessFSWriteFile,
		"LANDLOCK_ACCESS_FS_READ_FILE": AccessFSReadFile, "LANDLOCK_ACCESS_FS_READ_DIR": AccessFSReadDir,
		"LANDLOCK_ACCESS_FS_REMOVE_DIR": AccessFSRemoveDir, "LANDLOCK_ACCESS_FS_REMOVE_FILE": AccessFSRemoveFile,
		"LANDLOCK_ACCESS_FS_MAKE_CHAR": AccessFSMakeChar, "LANDLOCK_ACCESS_FS_MAKE_DIR": AccessFSMakeDir,
		"LANDLOCK_ACCESS_FS_MAKE_REG": AccessFSMakeReg, "LANDLOCK_ACCESS_FS_MAKE_SOCK": AccessFSMakeSock,
		"LANDLOCK_ACCESS_FS_MAKE_FIFO": AccessFSMakeFifo, "LANDLOCK_ACCESS_FS_MAKE_BLOCK": AccessFSMakeBlock,
		"LANDLOCK_ACCESS_FS_MAKE_SYM": AccessFSMakeSym, "LANDLOCK_ACCESS_FS_REFER": AccessFSRefer,
		"LANDLOCK_ACCESS_FS_TRUNCATE": AccessFSTruncate, "LANDLOCK_ACCESS_FS_IOCTL_DEV": AccessFSIoctlDev,
		"LANDLOCK_ACCESS_FS_RESOLVE_UNIX": AccessFSResolveUnix,
	}
	for name, want := range fs {
		if got[name] != want {
			t.Errorf("%s = %#x, want %#x", name, uint64(got[name]), uint64(want))
		}
	}
	if AccessNetBindTCP != 1<<0 || AccessNetConnectTCP != 1<<1 {
		t.Error("LANDLOCK_ACCESS_NET_{BIND,CONNECT}_TCP drifted")
	}
	if ScopeAbstractUnixSocket != 1<<0 || ScopeSignal != 1<<1 {
		t.Error("LANDLOCK_SCOPE_* drifted")
	}
	if HandledFS != (1<<17)-1 {
		t.Errorf("HandledFS = %#x, want every ABI 9 right (%#x) and nothing newer", uint64(HandledFS), (1<<17)-1)
	}
	if RequiredABI != 9 {
		t.Error("RESOLVE_UNIX is ABI 9: the required ABI must say so")
	}
}

func TestAccessClasses(t *testing.T) {
	if ReadWrite&(AccessFSMakeChar|AccessFSMakeBlock|AccessFSIoctlDev|AccessFSResolveUnix) != 0 {
		t.Fatal("ReadWrite must grant no device creation, device ioctl or pathname-socket access")
	}
	if FileRights&^(AccessFSExecute|AccessFSWriteFile|AccessFSReadFile|AccessFSTruncate|AccessFSIoctlDev) != 0 {
		t.Fatal("FileRights carries a directory-only right")
	}
	if (Rule{Access: ReadWrite, IsDir: false}).Effective()&AccessFSReadDir != 0 {
		t.Fatal("a file rule kept a directory right")
	}
	if !slices.Equal(Device.Names(), []string{"write_file", "read_file", "ioctl_dev"}) {
		t.Fatalf("Device = %v", Device.Names())
	}
	if !slices.Equal(Scopes().Names(), []string{"abstract_unix_socket", "signal"}) {
		t.Fatalf("Scopes = %v", Scopes().Names())
	}
}

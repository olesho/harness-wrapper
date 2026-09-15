//go:build linux

package main

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// seccompDenyLandlockExec is fault injection for the "seccomp blocks
// Landlock" refusal cell: it makes the three Landlock syscalls fail with
// ENOSYS for argv and everything it starts, then execs argv. The filter is
// installed on the locked thread that calls execve, so the new image
// inherits it; no container runtime is needed inside the guest.
func seccompDenyLandlockExec(argv []string) int {
	if len(argv) == 0 {
		fmt.Fprintln(os.Stderr, "usage: kernelprobe -seccomp-deny-landlock -- CMD [ARGS...]")
		return 2
	}
	path, err := exec.LookPath(argv[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	runtime.LockOSThread() // exec replaces the process; never unlocked

	arch := uint32(0xc000003e) // AUDIT_ARCH_X86_64
	if runtime.GOARCH == "arm64" {
		arch = 0xc00000b7 // AUDIT_ARCH_AARCH64
	}
	const (
		ldW    = unix.BPF_LD | unix.BPF_W | unix.BPF_ABS
		jeqK   = unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K
		retK   = unix.BPF_RET | unix.BPF_K
		allow  = unix.SECCOMP_RET_ALLOW
		enosys = unix.SECCOMP_RET_ERRNO | uint32(unix.ENOSYS)
	)
	prog := []unix.SockFilter{
		{Code: ldW, K: 4},                   // seccomp_data.arch
		{Code: jeqK, Jt: 0, Jf: 4, K: arch}, // other arch: allow
		{Code: ldW, K: 0},                   // seccomp_data.nr
		{Code: jeqK, Jt: 3, Jf: 0, K: unix.SYS_LANDLOCK_CREATE_RULESET},
		{Code: jeqK, Jt: 2, Jf: 0, K: unix.SYS_LANDLOCK_ADD_RULE},
		{Code: jeqK, Jt: 1, Jf: 0, K: unix.SYS_LANDLOCK_RESTRICT_SELF},
		{Code: retK, K: allow},
		{Code: retK, K: enosys},
	}
	fprog := unix.SockFprog{Len: uint16(len(prog)), Filter: &prog[0]}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		fmt.Fprintln(os.Stderr, "no_new_privs:", err)
		return 2
	}
	if _, _, e := unix.Syscall(unix.SYS_SECCOMP, unix.SECCOMP_SET_MODE_FILTER, 0, uintptr(unsafe.Pointer(&fprog))); e != 0 {
		fmt.Fprintln(os.Stderr, "seccomp:", e)
		return 2
	}
	err = syscall.Exec(path, argv, os.Environ())
	fmt.Fprintln(os.Stderr, "exec:", err)
	return 2
}

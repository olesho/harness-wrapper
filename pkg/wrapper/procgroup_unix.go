//go:build unix

package wrapper

import (
	"errors"
	"os"
	"syscall"
)

// getpgidFn and killFn indirect the two syscalls group termination makes so
// tests can stub a degenerate or self-referential process group and assert the
// per-process fallback is taken. Production code never reassigns them.
var (
	getpgidFn = syscall.Getpgid
	killFn    = syscall.Kill
)

// resolveSessionGroup returns the process group led by pid, or 0 when that
// group must not be signalled.
//
// Sessions start under pty.Start, which sets SysProcAttr.Setsid, so the harness
// is a session leader whose PGID equals its PID and every tool subprocess it
// spawns inherits that group. Anything else — a lookup failure, a group the
// harness does not lead, or a dangerous group (see signalableGroup) — resolves
// to 0, and the wrapper signals the harness process alone, as it did before
// group-scoped termination.
func resolveSessionGroup(pid int) int {
	pgid, err := getpgidFn(pid)
	if err != nil || pgid != pid || !signalableGroup(pgid) {
		return 0
	}
	return pgid
}

// signalableGroup reports whether pgid is a process group the wrapper may
// signal. It mirrors the self-protection in loom's own process-tree reaper:
// never group 0, never init's group, never our own — signalling the wrapper's
// group would take down the caller (the loom daemon) with the harness.
func signalableGroup(pgid int) bool {
	if pgid <= 1 {
		return false
	}
	return pgid != syscall.Getpgrp()
}

// killGroup delivers sig to every member of process group pgid.
func killGroup(pgid int, sig syscall.Signal) error {
	return killFn(-pgid, sig)
}

// groupHasMembers reports whether process group pgid still has any member.
// EPERM means members exist that this process may not signal.
func groupHasMembers(pgid int) bool {
	err := killFn(-pgid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// ignoreProcessGone maps "the target is already gone" onto success.
func ignoreProcessGone(err error) error {
	if errors.Is(err, syscall.ESRCH) || errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	return err
}

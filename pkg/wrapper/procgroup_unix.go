//go:build unix

package wrapper

import (
	"errors"
	"os"
	"syscall"
)

// getpgidFn and killFn indirect the two syscalls signalProcessGroup makes so
// tests can stub a degenerate or self-referential process group and assert the
// per-process fallback is taken. Production code never reassigns them.
var (
	getpgidFn = syscall.Getpgid
	killFn    = syscall.Kill
)

// signalProcessGroup delivers sig to the whole process group led by p, not
// just to p itself.
//
// This is sound because sessions are started with pty.Start, which sets
// SysProcAttr.Setsid: the harness is therefore a session leader whose PGID
// equals its PID, and every tool subprocess it spawns inherits that group.
// Signalling the PID alone leaves those descendants running — reparented to
// PID 1, with their exit status unreachable by anyone — which is exactly how a
// 35-minute test run outlived the session that started it.
//
// Safety: group 0 means "the caller's own group", group 1 is init's, and the
// wrapper's own group would take down the caller (the loom daemon) with the
// harness. Any of those, or a failure to resolve the group at all, falls back
// to signalling the single process, which is what the wrapper did before.
//
// ESRCH is swallowed: a process that already exited is a successful
// termination, and a signal error must never change a run's classification.
func signalProcessGroup(p *os.Process, sig syscall.Signal) error {
	if p == nil {
		return nil
	}
	pgid, err := getpgidFn(p.Pid)
	if err != nil || !signalableGroup(pgid) {
		return ignoreProcessGone(p.Signal(sig))
	}
	return ignoreProcessGone(killFn(-pgid, sig))
}

// signalableGroup reports whether pgid is a process group the wrapper may
// signal. It mirrors the self-protection in loom's own process-tree reaper:
// never group 0, never init's group, never our own.
func signalableGroup(pgid int) bool {
	if pgid <= 1 {
		return false
	}
	return pgid != syscall.Getpgrp()
}

// ignoreProcessGone maps "the target is already gone" onto success.
func ignoreProcessGone(err error) error {
	if errors.Is(err, syscall.ESRCH) || errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	return err
}

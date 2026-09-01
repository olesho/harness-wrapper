package main

import (
	"fmt"
	"os"
	"sort"
	"strings"
)

// runTmuxReap implements `harness-wrapper reap [--dry-run]`: a janitor for
// harness-wrapper sessions that have outlived their process.
//
// A session qualifies only when EVERY one of its panes reports pane_dead=1 --
// i.e. it holds no running process at all and exists purely as retained tmux
// state. Sessions with any live pane, and sessions without the hw- prefix, are
// never touched.
//
// Scope note: this deliberately has no cross-socket reach. Sessions stranded on
// the user's DEFAULT socket by builds predating the dedicated-socket change are
// not reaped here; clear those once by hand with
// `tmux kill-session -t hw-<name>`. A tool that goes hunting for sessions to
// kill on the user's own tmux server is a worse bug than the one it fixes.
func runTmuxReap(args []string) int {
	dryRun := false
	for _, a := range args {
		switch a {
		case "--dry-run":
			dryRun = true
		default:
			fmt.Fprintln(os.Stderr, "usage: harness-wrapper reap [--dry-run]")
			return 2
		}
	}
	if err := requireTmux(); err != nil {
		fmt.Fprintln(os.Stderr, tmuxErrPrefix, err)
		return 1
	}

	out, err := tmuxCmd("list-panes", "-a", "-F", "#{session_name} #{pane_dead}").Output()
	if err != nil {
		// tmux exits non-zero when no server is running on our socket. That is
		// the "nothing to reap" case, not an error -- same treatment as
		// runTmuxList.
		return 0
	}

	dead := deadHWSessions(string(out))
	if len(dead) == 0 {
		return 0
	}
	exit := 0
	for _, name := range dead {
		if dryRun {
			fmt.Printf("would reap: %s\n", name)
			continue
		}
		if err := tmuxCmd("kill-session", "-t", tmuxSessionPrefix+name).Run(); err != nil {
			fmt.Fprintf(os.Stderr, "harness-wrapper: reap %s: %v\n", name, err)
			exit = 1
			continue
		}
		fmt.Printf("reaped: %s\n", name)
	}
	return exit
}

// deadHWSessions parses `tmux list-panes -a -F '#{session_name} #{pane_dead}'`
// output and returns the bare names (hw- prefix stripped, sorted) of the
// harness-wrapper sessions whose panes are ALL dead.
//
// Split out as a pure function so the reap policy -- the part that decides what
// gets killed -- is testable without a tmux server.
func deadHWSessions(listPanesOutput string) []string {
	// nil = seen only dead panes so far; false pins the session as live.
	allDead := map[string]bool{}
	for _, line := range strings.Split(listPanesOutput, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			// Blank or malformed line: no session can be judged from it, so
			// skip rather than guess. Guessing here kills things.
			continue
		}
		session, deadFlag := fields[0], fields[1]
		if !strings.HasPrefix(session, tmuxSessionPrefix) {
			continue
		}
		isDead := deadFlag == "1"
		if prev, ok := allDead[session]; ok {
			allDead[session] = prev && isDead
			continue
		}
		allDead[session] = isDead
	}

	var names []string
	for session, dead := range allDead {
		if dead {
			names = append(names, strings.TrimPrefix(session, tmuxSessionPrefix))
		}
	}
	sort.Strings(names)
	return names
}

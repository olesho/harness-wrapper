package main

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/olesho/harness-wrapper/pkg/wrapper"
)

func TestContainFlagsRequireContain(t *testing.T) {
	for _, argv := range [][]string{
		{"--contain-rw", "/w", "claude", "--"},
		{"--contain-restrict-tcp", "claude", "--"},
		{"--contain-allow-tcp", "443", "claude", "--"},
		{"--contain-state-dir", "/s", "codex", "--"},
	} {
		if _, err := parseHarnessWrapperArgs(argv); err == nil || !strings.Contains(err.Error(), "--contain") {
			t.Errorf("%v: err = %v", argv, err)
		}
	}
	if _, err := parseHarnessWrapperArgs([]string{"--contain-allow-tcp", "0", "--contain", "landlock", "claude", "--"}); err == nil {
		t.Error("port 0 accepted")
	}
}

func TestContainFlagsBuildRequest(t *testing.T) {
	a, err := parseHarnessWrapperArgs([]string{
		"--contain", "landlock", "--contain-rw", "/w1", "--contain-rw", "/w2", "--contain-ro", "/r",
		"--contain-restrict-tcp", "--contain-allow-tcp", "443", "--contain-min-abi", "10",
		"--contain-state-dir", "/state", "--contain-pass-env", "FOO", "claude", "--", "--x",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := &wrapper.Containment{
		Kind: "landlock", ReadWrite: []string{"/w1", "/w2"}, ReadOnly: []string{"/r"},
		RestrictTCP: true, ConnectTCP: []uint16{443}, MinABI: 10, StateDir: "/state", PassEnv: []string{"FOO"},
	}
	if got := a.Contain.request(); !reflect.DeepEqual(got, want) {
		t.Fatalf("request = %+v, want %+v", got, want)
	}
	if a.Contain.request() == nil || (harnessWrapperArgs{}).Contain.request() != nil {
		t.Fatal("request must be nil exactly when --contain is absent")
	}
}

// TestContainFlagsRoundTrip: the flags a request renders to (tmux re-exec,
// pkg/env's runner argv) parse back to the same request — a dropped option
// would run the pane or guest uncontained, or under another policy.
func TestContainFlagsRoundTrip(t *testing.T) {
	req := &wrapper.Containment{
		Kind: "landlock", ReadWrite: []string{"/w"}, ReadOnly: []string{"/r1", "/r2"},
		RestrictTCP: true, ConnectTCP: []uint16{443, 8443}, MinABI: 9, StateDir: "/s", PassEnv: []string{"A", "B"},
	}
	argv := append(req.CLIFlags(), "claude", "--")
	a, err := parseHarnessWrapperArgs(argv)
	if err != nil {
		t.Fatal(err)
	}
	if got := a.Contain.request(); !reflect.DeepEqual(got, req) {
		t.Fatalf("round trip = %+v, want %+v", got, req)
	}
	deny := &wrapper.Containment{Kind: "landlock", RestrictTCP: true}
	a, _ = parseHarnessWrapperArgs(append(deny.CLIFlags(), "codex", "--"))
	if got := a.Contain.request(); !got.RestrictTCP || len(got.ConnectTCP) != 0 {
		t.Fatalf("deny-all TCP lost in the round trip: %+v", got)
	}
}

func TestTmuxReexecForwardsContainment(t *testing.T) {
	a, err := parseHarnessWrapperArgs([]string{"--contain", "landlock", "--contain-ro", "/r", "--tmux-session", "s", "claude", "--", "--x"})
	if err != nil {
		t.Fatal(err)
	}
	argv := tmuxReexecArgv(a, "/bin/hw", "/tmp/t")
	i := slices.Index(argv, "--contain")
	if i < 0 || argv[i+1] != "landlock" || !slices.Contains(argv, "--contain-ro") || slices.Index(argv, "claude") < i {
		t.Fatalf("pane argv lost containment: %v", argv)
	}
	plain, _ := parseHarnessWrapperArgs([]string{"--tmux-session", "s", "claude", "--"})
	for _, tok := range tmuxReexecArgv(plain, "/bin/hw", "/tmp/t") {
		if strings.HasPrefix(tok, "--contain") {
			t.Fatalf("uncontained pane argv carries %s", tok)
		}
	}
}

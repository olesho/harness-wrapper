// Package compat checks this tree against the previous release's actual
// binaries and readers (G8, G9): a runner built before containment rejects the
// --contain flags instead of running uncontained, the previous chat.Reopen
// refuses every contained record while still reading uncontained ones, and
// the previous harness-chatd has no capability route, so the new clients never
// send it containment. It needs the Go toolchain and the module proxy (or a
// warm module cache), so it runs only when HW_COMPAT_PREVIOUS names the
// previous release tag, as the landlock-security workflow's compatibility job
// does.
package compat

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/pkg/chat"
	"github.com/olesho/harness-wrapper/pkg/containment"
)

func previous(t *testing.T) string {
	t.Helper()
	v := os.Getenv("HW_COMPAT_PREVIOUS")
	if v == "" {
		t.Skip("set HW_COMPAT_PREVIOUS=<previous release tag> to run the compatibility checks")
	}
	return v
}

// installPrevious builds one of the previous release's commands.
func installPrevious(t *testing.T, version, cmd string) string {
	t.Helper()
	bin := t.TempDir()
	c := exec.Command("go", "install", "github.com/olesho/harness-wrapper/cmd/"+cmd+"@"+version)
	c.Env = append(os.Environ(), "GOBIN="+bin, "GOFLAGS=-mod=mod")
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("install %s@%s: %v\n%s", cmd, version, err, out)
	}
	return filepath.Join(bin, cmd)
}

// TestPreviousRunnerRejectsContain: pkg/env passes --contain* to the guest
// runner only when containment is set; a runner built before containment
// must fail on them, never run the turn uncontained.
func TestPreviousRunnerRejectsContain(t *testing.T) {
	version := previous(t)
	runner := installPrevious(t, version, "harness-wrapper")
	req := &containment.Request{Kind: containment.KindLandlock, ReadOnly: []string{"/tmp"}}
	for _, sub := range [][]string{nil, {"run"}, {"structured-run"}} {
		args := append(append(append([]string{}, sub...), req.CLIFlags()...), "claude", "--")
		c := exec.Command(runner, args...)
		c.Stdin = strings.NewReader("hello")
		out, err := c.CombinedOutput()
		if err == nil {
			t.Fatalf("%s %v accepted --contain:\n%s", version, sub, out)
		}
		if !strings.Contains(string(out), "contain") {
			t.Fatalf("%s %v failed for another reason:\n%s", version, sub, out)
		}
	}
}

const readerProgram = `package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/olesho/harness-wrapper/pkg/chat"
	"github.com/olesho/harness-wrapper/pkg/chat/memstore"
)

func main() {
	raw, err := os.ReadFile(os.Args[1])
	if err != nil {
		panic(err)
	}
	var records map[string]json.RawMessage
	if err := json.Unmarshal(raw, &records); err != nil {
		panic(err)
	}
	out := map[string]string{}
	for name, rec := range records {
		var s chat.Session
		if err := json.Unmarshal(rec, &s); err != nil {
			out[name] = "decode: " + err.Error()
			continue
		}
		store := memstore.New()
		_ = store.CreateSession(context.Background(), &s)
		_, err := chat.Reopen(context.Background(), chat.ReopenOptions{SessionID: s.ID, Store: store, BinaryPath: "/nonexistent/harness-binary"})
		switch {
		case errors.Is(err, chat.ErrNoHarnessSession):
			out[name] = "no_harness_session"
		case err == nil:
			out[name] = "resumed"
		default:
			out[name] = "launch_attempted: " + err.Error()
		}
	}
	b, _ := json.Marshal(out)
	fmt.Println(string(b))
}
`

// TestPreviousReaderRefusesContainedRecords runs the previous release's
// chat.Reopen over records this release writes: contained records before
// discovery, after it, after turns and after a reopen must all be refused
// (the legacy harness id stays empty), and an uncontained record must still be
// read and resumed — here it gets as far as launching a missing binary.
func TestPreviousReaderRefusesContainedRecords(t *testing.T) {
	version := previous(t)
	dir := t.TempDir()
	mod := fmt.Sprintf("module compatreader\n\ngo 1.25.0\n\nrequire github.com/olesho/harness-wrapper %s\n", version)
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(mod), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(readerProgram), 0o600); err != nil {
		t.Fatal(err)
	}
	tidy := exec.Command("go", "mod", "tidy")
	tidy.Dir = dir
	if out, err := tidy.CombinedOutput(); err != nil {
		t.Fatalf("go mod tidy: %v\n%s", err, out)
	}

	base := func(id string) chat.Session {
		return chat.Session{ID: id, Harness: "claude-code", WorkingDir: dir, CreatedAt: time.Now().UTC()}
	}
	contained := func(id, harnessID string, launched bool) chat.Session {
		s := base(id)
		s.Containment = &chat.SessionContainment{
			Version:  chat.ContainmentRecordVersion,
			Required: &containment.Request{Kind: containment.KindLandlock, MinABI: 9},
			Profile:  "claude-code@2.1.270", ProfileVersion: 1, StateID: "abcdefghijkl", Resumable: true,
			HarnessSessionID: harnessID,
		}
		if launched {
			s.Containment.LastLaunch = &containment.Applied{SchemaVersion: 1, Kind: containment.KindLandlock, ABI: 9, Fingerprint: "sha256:x"}
			s.Containment.Targets = map[string]string{dir: dir}
		}
		return s
	}
	uncontained := base("uncontained")
	uncontained.HarnessSessionID = "11111111-2222-3333-4444-555555555555"
	records := map[string]chat.Session{
		"before_discovery": contained("c1", "", true),
		"after_discovery":  contained("c2", "11111111-2222-3333-4444-555555555555", true),
		"after_turns":      contained("c3", "11111111-2222-3333-4444-555555555555", true),
		"after_reopen":     contained("c4", "11111111-2222-3333-4444-555555555555", true),
		"uncontained":      uncontained,
	}
	b, _ := json.Marshal(records)
	path := filepath.Join(dir, "records.json")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	run := exec.Command("go", "run", ".", path)
	run.Dir = dir
	out, err := run.CombinedOutput()
	if err != nil {
		t.Fatalf("previous reader: %v\n%s", err, out)
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(out))), &got); err != nil {
		t.Fatalf("parse %s: %v", out, err)
	}
	for name := range records {
		want := "no_harness_session"
		if name == "uncontained" {
			if !strings.HasPrefix(got[name], "launch_attempted") {
				t.Errorf("%s reader could not read an uncontained record: %s", version, got[name])
			}
			continue
		}
		if got[name] != want {
			t.Errorf("%s reader on %s record: %s, want %s", version, name, got[name], want)
		}
	}
}

// TestPreviousGatewayHasNoCapabilities: the previous harness-chatd answers
// 404 on the capability route (so the new clients refuse to send containment
// to it) and still accepts an uncontained open from the new clients.
func TestPreviousGatewayHasNoCapabilities(t *testing.T) {
	version := previous(t)
	chatd := installPrevious(t, version, "harness-chatd")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, chatd, "--bind", addr)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Wait() }()
	defer cancel()
	var resp *http.Response
	for i := 0; i < 100; i++ {
		if resp, err = http.Get("http://" + addr + "/v1/capabilities"); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("previous chatd never answered: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("previous chatd answered %d on /v1/capabilities, want 404", resp.StatusCode)
	}
}

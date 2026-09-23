package claudecode

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/olesho/harness-wrapper/pkg/transcript"
)

const followSession = "c0ffee00-0000-4000-8000-000000000001"

// claudeTranscriptPath is where claude, launched in workingDir with config
// root configDir, writes session id's transcript: under the realpath of
// workingDir.
func claudeTranscriptPath(t *testing.T, configDir, workingDir, id string) string {
	t.Helper()
	real, err := filepath.EvalSymlinks(workingDir)
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(configDir, "projects", EncodedCWD(real), id+".jsonl")
}

func appendTranscript(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestLocate: the transcript is found by the launch adapter's rules — the
// config root from the launch env's CLAUDE_CONFIG_DIR (the last occurrence,
// trimmed; relative to the working dir), else ~/.claude; the project directory
// named for the realpath of the working dir.
func TestLocate(t *testing.T) {
	home, wd, abs := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(wd, link); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name       string
		workingDir string
		env        []string
		root       string
	}{
		{"default", wd, []string{"PATH=/bin"}, filepath.Join(home, ".claude")},
		{"blank", wd, []string{"CLAUDE_CONFIG_DIR=  "}, filepath.Join(home, ".claude")},
		{"absolute", wd, []string{"CLAUDE_CONFIG_DIR=" + abs}, abs},
		{"relative to the working dir", wd, []string{"CLAUDE_CONFIG_DIR=cfg"}, filepath.Join(wd, "cfg")},
		{"last occurrence, trimmed", wd, []string{"CLAUDE_CONFIG_DIR=/nowhere", "CLAUDE_CONFIG_DIR= " + abs + " "}, abs},
		{"symlinked working dir", link, []string{"CLAUDE_CONFIG_DIR=" + abs}, abs},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want := claudeTranscriptPath(t, tc.root, tc.workingDir, followSession)
			appendTranscript(t, want, []byte("{}\n"))
			defer func() { _ = os.Remove(want) }()
			if got, err := Locate(followSession, tc.workingDir, tc.env); err != nil || got != want {
				t.Fatalf("Locate = %q, %v; want %q", got, err, want)
			}
		})
	}

	t.Run("nil env is this process's", func(t *testing.T) {
		t.Setenv("CLAUDE_CONFIG_DIR", abs)
		want := claudeTranscriptPath(t, abs, wd, followSession)
		appendTranscript(t, want, []byte("{}\n"))
		if got, err := Locate(followSession, wd, nil); err != nil || got != want {
			t.Fatalf("Locate = %q, %v; want %q", got, err, want)
		}
	})
	t.Run("missing", func(t *testing.T) {
		if got, err := Locate("no-such-session", wd, []string{"CLAUDE_CONFIG_DIR=" + abs}); got != "" || !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("Locate = %q, %v; want fs.ErrNotExist", got, err)
		}
		if _, err := Locate("", wd, nil); err == nil {
			t.Fatal("Locate accepted an empty session id")
		}
	})
}

// followFixtures are the transcripts the follower must read as Read does: the
// tool loop, entries with several content blocks, every recorded API-error
// line (tagged, so APIError must come through too), and the transcripts the
// corpus recorded from a live claude.
func followFixtures(t *testing.T) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	for _, name := range []string{"usage_toolloop.jsonl", "follow_blocks.jsonl"} {
		data, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		out[name] = data
	}
	lines, err := filepath.Glob("../../../test/corpus/apierror/claude-code/*/line.jsonl")
	if err != nil || len(lines) == 0 {
		t.Fatalf("no API-error corpus lines: %v", err)
	}
	var corpus []byte
	for _, path := range lines {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		corpus = append(corpus, data...)
	}
	out["apierror-corpus"] = corpus
	recorded, err := filepath.Glob("../../../test/corpus/claude-code/*/transcript.jsonl")
	if err != nil || len(recorded) == 0 {
		t.Fatalf("no recorded claude transcripts: %v", err)
	}
	for _, path := range recorded {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		out[filepath.Base(filepath.Dir(path))] = data
	}
	return out
}

// withoutNativeIDs is events with the identity cleared: the follower's and
// Read's differ by design, and everything else must not.
func withoutNativeIDs(events []transcript.Event) []transcript.Event {
	out := make([]transcript.Event, len(events))
	for i, e := range events {
		e.NativeID = ""
		out[i] = e
	}
	return out
}

// TestFollow_MatchesRead is the follower's conformance with Read: claude
// appends its transcript in pieces of any size, the follower is polled between
// them and restarted from its committed checkpoint now and then, and the
// events it commits are Read's for the finished file — the same content, in
// the same order, numbered with the same Seqs. Only the identities differ.
func TestFollow_MatchesRead(t *testing.T) {
	for name, data := range followFixtures(t) {
		for _, chunk := range []int{1, 13, 256, 4096, len(data)} {
			t.Run(fmt.Sprintf("%s/chunk=%d", name, chunk), func(t *testing.T) {
				cfg, wd := t.TempDir(), t.TempDir()
				env := []string{"CLAUDE_CONFIG_DIR=" + cfg}
				f, err := Follow(followSession, wd, env, transcript.Checkpoint{})
				if err != nil {
					t.Fatal(err)
				}
				var got []transcript.Event
				polls := 0
				for pos := 0; pos < len(data); pos += chunk {
					appendTranscript(t, f.Path(), data[pos:min(pos+chunk, len(data))])
					for {
						b, err := f.Poll()
						if err != nil {
							t.Fatalf("Poll: %v", err)
						}
						if len(b.Errors) != 0 {
							t.Fatalf("source errors on a clean transcript: %v", b.Errors)
						}
						if b.Checkpoint == b.From {
							break
						}
						for _, fe := range b.Events {
							got = append(got, fe.Event)
						}
						if err := f.Ack(b); err != nil {
							t.Fatal(err)
						}
					}
					if polls++; polls%7 == 0 {
						if f, err = Follow(followSession, wd, env, f.Checkpoint()); err != nil {
							t.Fatal(err)
						}
					}
				}
				want, err := (&Reader{ProjectsRoot: filepath.Join(cfg, "projects")}).Read(followSession, wd)
				if err != nil {
					t.Fatal(err)
				}
				if len(want) == 0 || !reflect.DeepEqual(withoutNativeIDs(got), withoutNativeIDs(want)) {
					t.Fatalf("followed events differ from Read:\n got %+v\nwant %+v", got, want)
				}
			})
		}
	}
}

// TestFollow_BlockIdentities: an entry with several content blocks gives each
// event its own identity, by the entry's uuid and the block's index in its
// content — skipped blocks (thinking) keep their index — or by the tool-use id.
// Read's legacy ids for the same events number text by position instead, and
// stay as they were.
func TestFollow_BlockIdentities(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "follow_blocks.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	cfg, wd := t.TempDir(), t.TempDir()
	f, err := Follow(followSession, wd, []string{"CLAUDE_CONFIG_DIR=" + cfg}, transcript.Checkpoint{})
	if err != nil {
		t.Fatal(err)
	}
	appendTranscript(t, f.Path(), data)
	b, err := f.Poll()
	if err != nil {
		t.Fatal(err)
	}
	const u = "b1000000-0000-4000-8000-00000000000"
	want := []struct{ follower, legacy string }{
		{"v1:text:line:" + u + "1:0", "file:text:" + u + "1:0"},
		{"v1:text:line:" + u + "2:1", "file:text:" + u + "2:1"},
		{"v1:tool_use:tool:toolu_01LIST", "tool-use:toolu_01LIST"},
		{"v1:text:line:" + u + "2:3", "file:text:" + u + "2:3"},
		{"v1:tool_result:tool:toolu_01LIST", "tool-result:toolu_01LIST"},
		{"v1:text:line:" + u + "3:1", "file:text:" + u + "3:5"},
		{"v1:text:line:" + u + "4:0", "file:text:" + u + "4:6"},
		{"v1:text:line:" + u + "4:1", "file:text:" + u + "4:7"},
		{"v1:text:line:" + u + "6:0", "file:text:" + u + "6:8"},
		{"v1:text:line:" + u + "7:0", "file:text:" + u + "7:9"},
	}
	legacy, err := Events(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Events) != len(want) || len(legacy) != len(want) {
		t.Fatalf("got %d followed and %d read events, want %d", len(b.Events), len(legacy), len(want))
	}
	for i, w := range want {
		if got := b.Events[i].Event.ID(); got != w.follower {
			t.Errorf("event %d: follower id %q, want %q", i, got, w.follower)
		}
		if got := legacy[i].ID(); got != w.legacy {
			t.Errorf("event %d: Read id %q, want %q", i, got, w.legacy)
		}
	}
	if last := b.Events[len(b.Events)-1].Event; last.APIError != "server_error" {
		t.Errorf("the API-error entry lost its tag: %+v", last)
	}
}

// TestFollow_ReportsWhatReadSkips: an entry that is not JSON, and user or
// assistant entries whose message cannot be read, are source errors — Read
// passes over them without a word — while every readable entry's events
// match Read's. Entries of other types hold no events and are no error,
// however they are shaped: the system api_error entry claude writes for each
// retry of a failed API call carries an object where Line expects the string
// "error".
func TestFollow_ReportsWhatReadSkips(t *testing.T) {
	lines := []string{
		`{"type":"user","uuid":"u1","message":{"role":"user","content":"hello"}}`,
		`{"type":"user","uuid":"u2","message":{"role":"user","content":42}}`,
		`{"type":"assistant","uuid":"a1","message":"not an object"}`,
		`{"type":"assistant","uuid":"a2","message":{"content":[{"type":"text","text":"hi`,
		`{"type":"system","uuid":"s1"}`,
		`{"type":"system","subtype":"api_error","level":"error","uuid":"s2","error":{"message":"529 Overloaded","status":529},"retryInMs":1000,"retryAttempt":1,"maxRetries":10}`,
		`{"type":"assistant","uuid":"a3","message":{"role":"assistant","content":[{"type":"text","text":"hi"}]}}`,
	}
	data := []byte(strings.Join(lines, "\n") + "\n")
	cfg, wd := t.TempDir(), t.TempDir()
	f, err := Follow(followSession, wd, []string{"CLAUDE_CONFIG_DIR=" + cfg}, transcript.Checkpoint{})
	if err != nil {
		t.Fatal(err)
	}
	appendTranscript(t, f.Path(), data)
	b, err := f.Poll()
	if err != nil {
		t.Fatal(err)
	}
	var reasons []string
	for _, se := range b.Errors {
		reasons = append(reasons, se.Err.Error())
	}
	if len(b.Errors) != 3 ||
		!strings.Contains(reasons[0], "neither text nor a block array") ||
		!strings.Contains(reasons[1], "assistant entry") ||
		!strings.Contains(reasons[2], "malformed transcript line") {
		t.Fatalf("source errors = %q; want the three unreadable entries, each saying why", reasons)
	}
	read, err := Events(data)
	if err != nil {
		t.Fatal(err)
	}
	var got []transcript.Event
	for _, fe := range b.Events {
		got = append(got, fe.Event)
	}
	if len(read) != 2 || !reflect.DeepEqual(withoutNativeIDs(got), withoutNativeIDs(read)) {
		t.Fatalf("followed events %+v, Read's %+v", got, read)
	}
}

// TestFollow_RepeatedEntryKeepsItsIdentity: claude can write an entry it
// already wrote again, byte for byte, further down the same file — seen after a
// session resumes. The repeat carries the first one's identity, so a store
// that keeps one event per identity keeps it once; Read, whose text ids count
// positions, returns it twice.
func TestFollow_RepeatedEntryKeepsItsIdentity(t *testing.T) {
	entry := `{"type":"assistant","uuid":"a1","message":{"role":"assistant","content":[{"type":"text","text":"hi"},{"type":"tool_use","id":"toolu_1","name":"Bash","input":{}}]}}`
	data := []byte(entry + "\n" + `{"type":"permission-mode","permissionMode":"default"}` + "\n" + entry + "\n")
	cfg, wd := t.TempDir(), t.TempDir()
	f, err := Follow(followSession, wd, []string{"CLAUDE_CONFIG_DIR=" + cfg}, transcript.Checkpoint{})
	if err != nil {
		t.Fatal(err)
	}
	appendTranscript(t, f.Path(), data)
	b, err := f.Poll()
	if err != nil || len(b.Events) != 4 {
		t.Fatalf("Poll = %+v, %v; want both copies' events", b, err)
	}
	for i := range 2 {
		if first, again := b.Events[i].Event.ID(), b.Events[i+2].Event.ID(); first != again {
			t.Errorf("block %d: the repeat's identity %q differs from the first's %q", i, again, first)
		}
	}
	read, _ := Events(data)
	if len(read) != 4 || read[0].ID() == read[2].ID() {
		t.Fatalf("Read = %+v; want both copies, the text ids apart", read)
	}
}

// TestFollow_WaitsForTheTranscript: claude creates its transcript with the
// first entry, so a follower started at launch waits where claude will write
// it — under the realpath of the working dir — and Poll reports
// fs.ErrNotExist until then.
func TestFollow_WaitsForTheTranscript(t *testing.T) {
	cfg, wd := t.TempDir(), t.TempDir()
	f, err := Follow(followSession, wd, []string{"CLAUDE_CONFIG_DIR=" + cfg}, transcript.Checkpoint{})
	if err != nil {
		t.Fatal(err)
	}
	if want := claudeTranscriptPath(t, cfg, wd, followSession); f.Path() != want {
		t.Fatalf("Path = %q, want %q", f.Path(), want)
	}
	if _, err := f.Poll(); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Poll before the transcript exists = %v; want fs.ErrNotExist", err)
	}
	appendTranscript(t, f.Path(), []byte(`{"type":"user","uuid":"u1","message":{"role":"user","content":"hello"}}`+"\n"))
	if b, err := f.Poll(); err != nil || len(b.Events) != 1 {
		t.Fatalf("Poll once it exists = %+v, %v", b, err)
	}
}

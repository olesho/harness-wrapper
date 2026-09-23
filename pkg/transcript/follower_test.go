package transcript

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/olesho/harness-wrapper/internal/sessionid"
)

// toyRecord is the record format of the tests' toy harness: text blocks, then
// an optional tool call or result, under an optional uuid.
type toyRecord struct {
	UUID       string   `json:"uuid,omitempty"`
	Texts      []string `json:"texts,omitempty"`
	ToolUse    string   `json:"tool_use,omitempty"`
	ToolResult string   `json:"tool_result,omitempty"`
}

func toyDecode(record []byte) ([]BlockEvent, error) {
	var r toyRecord
	if err := json.Unmarshal(record, &r); err != nil {
		return nil, err
	}
	var out []BlockEvent
	for i, text := range r.Texts {
		if text == "" {
			continue // a block that holds no event, as a thinking block
		}
		out = append(out, BlockEvent{Block: i, Event: Event{Role: RoleAssistant, Type: EventText, Text: text, UUID: r.UUID, Source: SourceFile}})
	}
	if r.ToolUse != "" {
		out = append(out, BlockEvent{Block: len(r.Texts), Event: Event{Role: RoleAssistant, Type: EventToolUse, ToolUseID: r.ToolUse, UUID: r.UUID, Source: SourceFile}})
	}
	if r.ToolResult != "" {
		out = append(out, BlockEvent{Block: len(r.Texts), Event: Event{Role: RoleTool, Type: EventToolResult, ToolUseID: r.ToolResult, UUID: r.UUID, Source: SourceFile}})
	}
	return out, nil
}

func toyLine(r toyRecord) string {
	b, err := json.Marshal(r)
	if err != nil {
		panic(err)
	}
	return string(b) + "\n"
}

// toyTranscript is n records of every shape: multi-block text, tool calls and
// results, records with no uuid, and long ones, so it outgrows the checkpoint
// windows. tag makes the ids and texts of one transcript its own.
func toyTranscript(n int, tag string) string {
	var sb strings.Builder
	for i := range n {
		switch i % 5 {
		case 0:
			sb.WriteString(toyLine(toyRecord{UUID: fmt.Sprintf("%su-%d", tag, i), Texts: []string{fmt.Sprintf("%sprompt %d", tag, i)}}))
		case 1:
			sb.WriteString(toyLine(toyRecord{UUID: fmt.Sprintf("%sa-%d", tag, i), Texts: []string{"first block", "", strings.Repeat("long ", 60)}, ToolUse: fmt.Sprintf("%stool-%d", tag, i)}))
		case 2:
			sb.WriteString(toyLine(toyRecord{UUID: fmt.Sprintf("%sr-%d", tag, i), ToolResult: fmt.Sprintf("%stool-%d", tag, i-1)}))
		case 3:
			sb.WriteString(toyLine(toyRecord{Texts: []string{fmt.Sprintf("%sno uuid %d", tag, i), "second"}}))
		case 4:
			sb.WriteString(toyLine(toyRecord{UUID: fmt.Sprintf("%sa-%d", tag, i), Texts: []string{fmt.Sprintf("%sreply %d", tag, i)}}))
		}
	}
	return sb.String()
}

// store is the caller's durable side: the events it committed, deduped by
// identity as a unique key would, the source errors, and the checkpoint — all
// written together, as one transaction would.
type store struct {
	cp     Checkpoint
	seen   map[string]bool
	events []Event
	errs   []SourceError
}

func newStore() *store { return &store{seen: map[string]bool{}} }

func (s *store) commit(b Batch) {
	for _, fe := range b.Events {
		if id := fe.Event.ID(); !s.seen[id] {
			s.seen[id] = true
			s.events = append(s.events, fe.Event)
		}
	}
	s.errs = append(s.errs, b.Errors...)
	s.cp = b.Checkpoint
}

// drain polls, commits and acknowledges until a poll reads nothing.
func drain(t *testing.T, f *Follower, s *store) {
	t.Helper()
	for {
		b, err := f.Poll()
		if err != nil {
			t.Fatalf("Poll: %v", err)
		}
		if b.Checkpoint == b.From {
			return
		}
		s.commit(b)
		if err := f.Ack(b); err != nil {
			t.Fatalf("Ack: %v", err)
		}
	}
}

func appendFile(t *testing.T, path, data string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(data); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func newToyFollower(t *testing.T, path string, from Checkpoint) *Follower {
	t.Helper()
	f, err := NewFollower(path, "session-1", from, toyDecode)
	if err != nil {
		t.Fatalf("NewFollower: %v", err)
	}
	return f
}

// followWhole reads the whole file in one pass under generation gen.
func followWhole(t *testing.T, path, gen string) []Event {
	t.Helper()
	f := newToyFollower(t, path, Checkpoint{SessionID: "session-1", Generation: gen})
	s := newStore()
	drain(t, f, s)
	return s.events
}

// jsonRoundTrip is cp as a caller reads it back from storage.
func jsonRoundTrip(t *testing.T, cp Checkpoint) Checkpoint {
	t.Helper()
	data, err := json.Marshal(cp)
	if err != nil {
		t.Fatal(err)
	}
	var back Checkpoint
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	return back
}

// checkWindows asserts a checkpoint's hashes describe the file's bytes.
func checkWindows(t *testing.T, data []byte, cp Checkpoint) {
	t.Helper()
	if want := min(cp.Offset, checkpointWindow); cp.PrefixLen != want || cp.BoundaryLen != want {
		t.Fatalf("windows = %d/%d at offset %d, want %d", cp.PrefixLen, cp.BoundaryLen, cp.Offset, want)
	}
	if got := sha256Hex(data[:cp.PrefixLen]); got != cp.PrefixSHA256 {
		t.Fatalf("prefix hash %s, file has %s", cp.PrefixSHA256, got)
	}
	if got := sha256Hex(data[cp.Offset-cp.BoundaryLen : cp.Offset]); got != cp.BoundarySHA256 {
		t.Fatalf("boundary hash %s, file has %s", cp.BoundarySHA256, got)
	}
}

// TestFollower_ChunkedAppends: the harness writes its transcript in pieces of
// any size, 1 B to 4 KiB, and the follower is polled — and now and then
// restarted from its stored checkpoint — between them. Whatever the pieces,
// it commits exactly the events, identities and Seqs of one read of the
// whole file, and its checkpoint hashes the file's own bytes.
func TestFollower_ChunkedAppends(t *testing.T) {
	data := toyTranscript(150, "")
	if len(data) < 3*checkpointWindow {
		t.Fatalf("the transcript is %d bytes; it must outgrow the checkpoint windows", len(data))
	}
	whole := filepath.Join(t.TempDir(), "whole.jsonl")
	appendFile(t, whole, data)
	gen := sessionid.NewUUID()
	want := followWhole(t, whole, gen)

	rng := rand.New(rand.NewSource(1))
	for _, size := range []int{1, 2, 3, 7, 64, 511, 1024, 4096, 0} {
		name := fmt.Sprintf("chunk=%d", size)
		if size == 0 {
			name = "chunk=random"
		}
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "s.jsonl")
			s := newStore()
			s.cp = Checkpoint{SessionID: "session-1", Generation: gen}
			f := newToyFollower(t, path, s.cp)
			polls := 0
			for pos := 0; pos < len(data); {
				n := size
				if n == 0 {
					n = 1 + rng.Intn(4096)
				}
				n = min(n, len(data)-pos)
				appendFile(t, path, data[pos:pos+n])
				pos += n
				drain(t, f, s)
				if polls++; polls%17 == 0 {
					f = newToyFollower(t, path, jsonRoundTrip(t, s.cp)) // restart from what was stored
				}
			}
			if !reflect.DeepEqual(s.events, want) {
				t.Fatalf("chunked follow differs from one read:\n got %d events %+v\nwant %d events %+v", len(s.events), s.events, len(want), want)
			}
			if s.cp.Offset != int64(len(data)) {
				t.Fatalf("offset = %d, want %d", s.cp.Offset, len(data))
			}
			checkWindows(t, []byte(data), s.cp)
		})
	}
}

// TestFollower_PartialRecordWaitsForItsNewline: bytes after the last newline
// are a record still being written. They are not read, and the offset stays at
// the last complete record, however the record ends.
func TestFollower_PartialRecordWaitsForItsNewline(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	line := toyLine(toyRecord{UUID: "u1", Texts: []string{"done"}})
	appendFile(t, path, strings.TrimSuffix(line, "\n")) // valid JSON, no newline yet
	f := newToyFollower(t, path, Checkpoint{})
	b, err := f.Poll()
	if err != nil || b.Checkpoint != b.From || len(b.Events)+len(b.Errors) != 0 {
		t.Fatalf("Poll of an unterminated record = %+v, %v; want nothing read", b, err)
	}
	appendFile(t, path, "\n"+`{"uuid":"u2","te`)
	b, err = f.Poll()
	if err != nil || len(b.Events) != 1 || b.Checkpoint.Offset != int64(len(line)) {
		t.Fatalf("Poll = %+v, %v; want the one complete record, offset %d", b, err, len(line))
	}
}

// TestFollower_PollDoesNotMoveUntilAck: a batch that is not acknowledged —
// its transaction failed, say — comes back from the next Poll with the same
// identities, record-less ids included, and Ack moves past it only once.
func TestFollower_PollDoesNotMoveUntilAck(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	appendFile(t, path, toyTranscript(4, ""))
	f := newToyFollower(t, path, Checkpoint{})
	first, err := f.Poll()
	if err != nil || len(first.Events) == 0 {
		t.Fatalf("Poll = %+v, %v", first, err)
	}
	again, err := f.Poll()
	if err != nil || !reflect.DeepEqual(first, again) {
		t.Fatalf("a second Poll without Ack differs:\n%+v\n%+v", first, again)
	}
	appendFile(t, path, toyLine(toyRecord{UUID: "late", Texts: []string{"late"}}))
	grown, err := f.Poll()
	if err != nil || !reflect.DeepEqual(grown.Events[:len(first.Events)], first.Events) || len(grown.Events) != len(first.Events)+1 {
		t.Fatalf("after an append the batch no longer starts with the unacknowledged one: %+v", grown)
	}
	if err := f.Ack(first); err != nil {
		t.Fatalf("Ack: %v", err)
	}
	if err := f.Ack(grown); err == nil {
		t.Fatal("Ack accepted a batch polled before the previous Ack")
	}
	rest, err := f.Poll()
	if err != nil || len(rest.Events) != 1 || rest.Events[0].Event.Text != "late" {
		t.Fatalf("Poll after Ack = %+v, %v; want just the appended record", rest, err)
	}
}

// TestFollower_CrashAroundCommit: the caller dies before its transaction
// commits, or after it commits but before Ack. Restarted from what it stored,
// it commits every event exactly once, with the identities one pass gives.
func TestFollower_CrashAroundCommit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	s := newStore()
	s.cp = Checkpoint{SessionID: "session-1", Generation: sessionid.NewUUID()}

	appendFile(t, path, toyTranscript(10, ""))
	f := newToyFollower(t, path, s.cp)
	lost, err := f.Poll() // crash before commit: nothing stored
	if err != nil || len(lost.Events) == 0 {
		t.Fatalf("Poll = %+v, %v", lost, err)
	}
	f = newToyFollower(t, path, s.cp)
	replay, err := f.Poll()
	if err != nil || !reflect.DeepEqual(replay, lost) {
		t.Fatalf("the replay after a crash before commit differs:\n%+v\n%+v", replay, lost)
	}
	s.commit(replay) // crash after commit, before Ack
	f = newToyFollower(t, path, s.cp)
	if b, err := f.Poll(); err != nil || b.Checkpoint != b.From {
		t.Fatalf("the committed batch came back after a restart: %+v, %v", b, err)
	}
	appendFile(t, path, toyLine(toyRecord{Texts: []string{"after the restart"}}))
	drain(t, f, s)

	if want := followWhole(t, path, s.cp.Generation); !reflect.DeepEqual(s.events, want) {
		t.Fatalf("stored events differ from one pass:\n got %+v\nwant %+v", s.events, want)
	}
}

// TestFollower_IdentityFormats pins the v1 identities: the tool-use id for a
// tool event, the record uuid and block for any other, and generation, offset
// and block for a record with neither. No two kinds share an identity.
func TestFollower_IdentityFormats(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	first := toyLine(toyRecord{UUID: "u1", Texts: []string{"a", "", "c"}, ToolUse: "t1"})
	appendFile(t, path, first+toyLine(toyRecord{UUID: "u2", ToolResult: "t1"})+toyLine(toyRecord{Texts: []string{"x", "y"}}))
	gen := sessionid.NewUUID()
	f := newToyFollower(t, path, Checkpoint{SessionID: "session-1", Generation: gen})
	b, err := f.Poll()
	if err != nil {
		t.Fatal(err)
	}
	third := len(first) + len(toyLine(toyRecord{UUID: "u2", ToolResult: "t1"}))
	want := []string{
		"v1:text:line:u1:0",
		"v1:text:line:u1:2",
		"v1:tool_use:tool:t1",
		"v1:tool_result:tool:t1",
		fmt.Sprintf("v1:text:gen:%s:%d:0", gen, third),
		fmt.Sprintf("v1:text:gen:%s:%d:1", gen, third),
	}
	var got []string
	for i, fe := range b.Events {
		got = append(got, fe.Event.NativeID)
		if fe.Event.ID() != fe.Event.NativeID || fe.Event.Seq != i {
			t.Fatalf("event %d: ID() = %q, Seq = %d", i, fe.Event.ID(), fe.Event.Seq)
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("identities:\n got %q\nwant %q", got, want)
	}
	if b.Events[4].Offset != int64(third) || b.Events[5].Block != 1 {
		t.Fatalf("positions = %+v", b.Events[4:])
	}
}

// resetCase mutates a followed transcript, data, in place or by replacing it.
type resetCase struct {
	name   string
	mutate func(t *testing.T, path, data string)
	noIno  bool // follow as a platform without inodes would
	want   ResetReason
}

// TestFollower_Resets: a transcript that no longer extends the checkpoint —
// shrunk, replaced by a larger file, truncated and regrown past the offset,
// or changed at its prefix or boundary — is a reset. Poll says why, keeps the
// previous checkpoint for evidence, and the follower starts a new generation:
// the next batch reads the new file from its first byte and numbers its
// events from 0.
func TestFollower_Resets(t *testing.T) {
	data := toyTranscript(40, "")
	other := toyTranscript(80, "other-") // a different, larger file
	flip := func(pos int) func(t *testing.T, path, data string) {
		return func(t *testing.T, path, data string) {
			b := []byte(data)
			b[pos] ^= 0x20 // same length, one byte differs
			if err := os.WriteFile(path, b, 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	cases := []resetCase{
		{"shrunk", func(t *testing.T, path, data string) {
			if err := os.Truncate(path, int64(len(data)/2)); err != nil {
				t.Fatal(err)
			}
		}, false, ResetShrunk},
		{"replaced-larger", func(t *testing.T, path, _ string) {
			tmp := path + ".new"
			appendFile(t, tmp, other)
			if err := os.Rename(tmp, path); err != nil {
				t.Fatal(err)
			}
		}, false, ResetReplaced},
		{"replaced-larger-no-inode", func(t *testing.T, path, _ string) {
			tmp := path + ".new"
			appendFile(t, tmp, other)
			if err := os.Rename(tmp, path); err != nil {
				t.Fatal(err)
			}
		}, true, ResetPrefixChanged},
		{"truncated-and-regrown", func(t *testing.T, path, _ string) {
			if err := os.Truncate(path, 0); err != nil {
				t.Fatal(err)
			}
			appendFile(t, path, other)
		}, false, ResetPrefixChanged},
		{"prefix-changed", flip(10), false, ResetPrefixChanged},
		{"boundary-changed", flip(len(data) - 10), false, ResetBoundaryChanged},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "s.jsonl")
			appendFile(t, path, data)
			s := newStore()
			f := newToyFollower(t, path, Checkpoint{})
			drain(t, f, s)
			if tc.noIno {
				s.cp.Inode = 0
				f = newToyFollower(t, path, s.cp)
			}
			tc.mutate(t, path, data)

			b, err := f.Poll()
			var reset *ResetError
			if !errors.As(err, &reset) || !errors.Is(err, ErrTranscriptReset) {
				t.Fatalf("Poll = %v; want a reset", err)
			}
			if reset.Reason != tc.want || reset.Previous != s.cp || b.Checkpoint != b.From || len(b.Events) != 0 {
				t.Fatalf("reset = %+v, batch %+v; want reason %s from %+v", reset, b, tc.want, s.cp)
			}
			now := f.Checkpoint()
			if now.Generation == s.cp.Generation || now.Offset != 0 || now.NextSeq != 0 || now.SessionID != s.cp.SessionID {
				t.Fatalf("after the reset the follower is at %+v", now)
			}
			next, err := f.Poll()
			content, _ := os.ReadFile(path)
			end := int64(strings.LastIndexByte(string(content), '\n') + 1)
			if err != nil || next.From != now || next.Checkpoint.Offset != end || len(next.Events) == 0 || next.Events[0].Event.Seq != 0 {
				t.Fatalf("the first batch of the new generation = %+v, %v; want every complete record, %d bytes, from Seq 0", next, err, end)
			}
		})
	}
}

// TestFollower_ResetReemitsNativeIdentities: after a reset the follower reads
// the file again from the start. Records with ids of their own come back with
// the identities already stored, so the caller's dedup drops them; records
// without belong to the new generation and are new events.
func TestFollower_ResetReemitsNativeIdentities(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	data := toyTranscript(10, "")
	appendFile(t, path, data)
	s := newStore()
	f := newToyFollower(t, path, Checkpoint{})
	drain(t, f, s)
	before := len(s.events)
	idless := 0
	for _, e := range s.events {
		if strings.Contains(e.NativeID, ":gen:") {
			idless++
		}
	}

	tmp := path + ".new" // the same records, rotated into a new file
	appendFile(t, tmp, data+toyLine(toyRecord{UUID: "new", Texts: []string{"new"}}))
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Poll(); !errors.Is(err, ErrTranscriptReset) {
		t.Fatalf("Poll = %v; want a reset", err)
	}
	drain(t, f, s)
	if got, want := len(s.events), before+idless+1; got != want || idless == 0 {
		t.Fatalf("after the reset the store holds %d events, want %d (%d before, %d without a native id, 1 new)", got, want, before, idless)
	}
}

// TestFollower_ReportsUnreadableRecords: a complete record the decoder cannot
// read is a SourceError with its offset, length and digest, carried by the
// batch that moves past it. Blank lines are skipped without one; an
// unreadable record still being written waits for its newline like any other.
func TestFollower_ReportsUnreadableRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	good := toyLine(toyRecord{UUID: "u1", Texts: []string{"ok"}})
	lines := []string{good, "{not json\n", "\n", "[1,2]\n", toyLine(toyRecord{UUID: "u2", Texts: []string{"still ok"}})}
	data := strings.Join(lines, "")
	appendFile(t, path, data+"{also not")
	f := newToyFollower(t, path, Checkpoint{})
	b, err := f.Poll()
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Events) != 2 || b.Events[1].Event.Seq != 1 || b.Checkpoint.Offset != int64(len(data)) {
		t.Fatalf("batch = %+v; want both readable records' events and the offset past all %d bytes", b, len(data))
	}
	wantAt := []int{len(good), len(good) + len("{not json\n") + 1}
	if len(b.Errors) != 2 {
		t.Fatalf("errors = %+v; want the two unreadable records", b.Errors)
	}
	for i, se := range b.Errors {
		rec := data[se.Offset : se.Offset+se.Length]
		sum := sha256.Sum256([]byte(rec))
		if se.Offset != int64(wantAt[i]) || !strings.HasSuffix(rec, "\n") || se.SHA256 != hex.EncodeToString(sum[:]) || se.Err == nil {
			t.Fatalf("error %d = %+v over %q", i, se, rec)
		}
		if !strings.Contains(se.Error(), fmt.Sprint(se.Offset)) {
			t.Fatalf("Error() = %q does not name the offset", se.Error())
		}
	}
}

// TestFollower_MaxBatchBytes: a batch stops at the last record boundary within
// the cap, except that a record longer than the cap comes whole on its own.
func TestFollower_MaxBatchBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	short := toyLine(toyRecord{UUID: "s", Texts: []string{"short"}})
	long := toyLine(toyRecord{UUID: "l", Texts: []string{strings.Repeat("x", 300)}})
	appendFile(t, path, short+short+short+long+short)
	f := newToyFollower(t, path, Checkpoint{})
	f.MaxBatchBytes = 2*len(short) + 5
	var sizes []int64
	for {
		b, err := f.Poll()
		if err != nil {
			t.Fatal(err)
		}
		if b.Checkpoint == b.From {
			break
		}
		sizes = append(sizes, b.Checkpoint.Offset-b.From.Offset)
		if err := f.Ack(b); err != nil {
			t.Fatal(err)
		}
	}
	want := []int64{int64(2 * len(short)), int64(len(short)), int64(len(long)), int64(len(short))}
	if !reflect.DeepEqual(sizes, want) {
		t.Fatalf("batch sizes = %v, want %v", sizes, want)
	}
}

// TestFollower_MissingFile: before the harness writes its transcript, Poll
// reports fs.ErrNotExist and reads nothing; once it exists, the follower
// reads it from the start.
func TestFollower_MissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	f := newToyFollower(t, path, Checkpoint{})
	b, err := f.Poll()
	if !errors.Is(err, fs.ErrNotExist) || b.Checkpoint != b.From {
		t.Fatalf("Poll of a missing file = %+v, %v", b, err)
	}
	appendFile(t, path, toyLine(toyRecord{UUID: "u1", Texts: []string{"hi"}}))
	if b, err := f.Poll(); err != nil || len(b.Events) != 1 {
		t.Fatalf("Poll once the file exists = %+v, %v", b, err)
	}
}

// TestCheckpoint_JSON pins the checkpoint's stored form: callers persist it
// and read it back across versions.
func TestCheckpoint_JSON(t *testing.T) {
	cp := Checkpoint{
		Version: 1, SessionID: "s", Generation: "g", Inode: 7, Offset: 10, NextSeq: 2,
		PrefixLen: 10, PrefixSHA256: "p", BoundaryLen: 10, BoundarySHA256: "b",
	}
	data, err := json.Marshal(cp)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"version":1,"session_id":"s","generation":"g","inode":7,"offset":10,"next_seq":2,` +
		`"prefix_len":10,"prefix_sha256":"p","boundary_len":10,"boundary_sha256":"b"}`
	if string(data) != want {
		t.Fatalf("JSON = %s\nwant   %s", data, want)
	}
	var back Checkpoint
	if err := json.Unmarshal(data, &back); err != nil || back != cp {
		t.Fatalf("round trip = %+v, %v", back, err)
	}
}

// TestNewFollower_Checkpoints: a fresh follower mints a generation; one
// resuming keeps the checkpoint's; a checkpoint for another session, from a
// newer follower, or with a position but no generation is refused.
func TestNewFollower_Checkpoints(t *testing.T) {
	f := newToyFollower(t, "s.jsonl", Checkpoint{})
	if cp := f.Checkpoint(); !sessionid.IsUUID(cp.Generation) || cp.SessionID != "session-1" || cp.Version != FollowerVersion {
		t.Fatalf("fresh checkpoint = %+v", cp)
	}
	resume := Checkpoint{Version: 1, SessionID: "session-1", Generation: "g", Offset: 5, PrefixLen: 5, BoundaryLen: 5}
	if f := newToyFollower(t, "s.jsonl", resume); f.Checkpoint() != resume || f.Offset() != 5 {
		t.Fatalf("resumed checkpoint = %+v", f.Checkpoint())
	}
	for name, cp := range map[string]Checkpoint{
		"other session":         {Version: 1, SessionID: "session-2", Generation: "g"},
		"newer version":         {Version: FollowerVersion + 1, SessionID: "session-1", Generation: "g"},
		"offset, no generation": {Version: 1, SessionID: "session-1", Offset: 5},
		"window past offset":    {Version: 1, SessionID: "session-1", Generation: "g", Offset: 5, PrefixLen: 6},
	} {
		if _, err := NewFollower("s.jsonl", "session-1", cp, toyDecode); err == nil {
			t.Errorf("%s: NewFollower accepted %+v", name, cp)
		}
	}
}

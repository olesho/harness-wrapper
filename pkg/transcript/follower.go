package transcript

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/olesho/harness-wrapper/internal/sessionid"
)

// FollowerVersion is the version of the rules a Follower identifies events
// and makes checkpoints by. Every identity it assigns starts "v1:", and every
// Checkpoint records it, so stored identities and checkpoints say which rules
// made them; a follower refuses a checkpoint from a newer version.
const FollowerVersion = 1

// ErrTranscriptReset reports that a transcript no longer extends the bytes a
// checkpoint covers: it shrank, was replaced, or changed at the checkpoint's
// prefix or boundary. Poll returns it as a *ResetError.
var ErrTranscriptReset = errors.New("transcript reset")

// A ResetReason says what a follower found in place of the bytes its
// checkpoint covers.
type ResetReason string

// The reasons a follower resets.
const (
	// ResetReplaced: a different file sits at the path (its inode changed).
	ResetReplaced ResetReason = "replaced"
	// ResetShrunk: the file is shorter than the checkpoint's offset.
	ResetShrunk ResetReason = "shrunk"
	// ResetPrefixChanged: the file's first bytes differ from the checkpoint's.
	ResetPrefixChanged ResetReason = "prefix_changed"
	// ResetBoundaryChanged: the bytes before the checkpoint's offset differ.
	ResetBoundaryChanged ResetReason = "boundary_changed"
)

// ResetError is the error Poll returns when the transcript no longer extends
// its checkpoint. It matches ErrTranscriptReset.
//
// A harness appends to its transcript, so a reset is a source-integrity fault
// — the file was rotated, truncated, or rewritten — and the caller records it
// as one, keeping Previous (and a copy of the file, if it wants the evidence)
// rather than trusting that the log only grows. The follower has already moved
// to a new generation at offset 0: the next Poll reads the file from its start,
// and the batch it returns carries the new generation's Checkpoint.
type ResetError struct {
	Reason   ResetReason
	Previous Checkpoint // the checkpoint the file no longer extends
	Size     int64      // the file's size when the reset was found
	Inode    uint64     // its inode then; 0 where the platform has none
}

func (e *ResetError) Error() string {
	return fmt.Sprintf("transcript reset (%s): session %s generation %s at offset %d, file now %d bytes",
		e.Reason, e.Previous.SessionID, e.Previous.Generation, e.Previous.Offset, e.Size)
}

// Unwrap makes a *ResetError match ErrTranscriptReset.
func (e *ResetError) Unwrap() error { return ErrTranscriptReset }

// A Checkpoint is how far a Follower has read one transcript, and enough
// about the bytes it read to tell, when it next looks, whether the file still
// extends them. The caller persists it in the same transaction as the events
// and source errors of the batch that proposed it. It is comparable, and its
// JSON form is stable.
//
// The prefix and boundary are what the follower checks: the file's first
// bytes, and the bytes just before Offset, each up to 4 KiB. Together with the
// size and inode they catch rotation, truncation and a truncate-and-regrow
// across a restart. They are not proof against rewriting in place: a change
// that keeps both windows and the size intact goes unseen.
type Checkpoint struct {
	// Version is the FollowerVersion that made the checkpoint.
	Version int `json:"version"`
	// SessionID is the harness session the transcript belongs to.
	SessionID string `json:"session_id"`
	// Generation names this incarnation of the file: a UUID the follower
	// mints when it starts on a file, and again after each reset. Records
	// with no native identity are identified within it.
	Generation string `json:"generation"`
	// Inode is the file's inode number; 0 where the platform has none.
	Inode uint64 `json:"inode,omitempty"`
	// Offset is how many bytes of the file the checkpoint covers. It always
	// falls just past a newline: a record is read whole or not at all.
	Offset int64 `json:"offset"`
	// NextSeq is the Seq of the next event. Events are numbered from 0 in
	// each generation, as Read numbers the events of a whole file.
	NextSeq int `json:"next_seq"`
	// PrefixLen and PrefixSHA256 describe the file's first bytes. The prefix
	// grows with Offset until it reaches 4 KiB, and is fixed from then on.
	PrefixLen    int64  `json:"prefix_len"`
	PrefixSHA256 string `json:"prefix_sha256"`
	// BoundaryLen and BoundarySHA256 describe the bytes just before Offset,
	// up to 4 KiB of them.
	BoundaryLen    int64  `json:"boundary_len"`
	BoundarySHA256 string `json:"boundary_sha256"`
}

// A Decoder turns one complete record of a harness transcript — a line,
// without its newline — into its events, each with the index of the content
// block it came from. It returns an error for a record it cannot read; the
// follower reports it as a SourceError. Decoders set every field of the event
// except Seq and NativeID, which the follower assigns.
type Decoder func(record []byte) ([]BlockEvent, error)

// A BlockEvent is one event a Decoder read from a record, and the index of
// the content block it came from: its position in the record's content array,
// 0 for a record with a single body.
type BlockEvent struct {
	Block int
	Event Event
}

// A FollowedEvent is one event of a Batch, and where in the file it came from.
//
// Event.NativeID holds the event's follower identity, so Event.ID() returns
// it. The identity is the most native one the record offers, qualified by the
// event's kind (Type) so that no two kinds ever share one:
//
//	v1:<kind>:tool:<tool-use id>                     tool_use / tool_result with a tool-use id
//	v1:<kind>:line:<record uuid>:<block>             any other event from a record with a uuid
//	v1:<kind>:gen:<generation>:<offset>:<block>      an event from a record with neither
//
// The first two are the harness's own ids, so they are the same across
// batches, restarts and resets: a file read again after a reset re-emits the
// identities already stored, and the caller's dedup drops them. The last is
// only as stable as the generation — a reset starts a new one — because a
// record with no id of its own is known only by where it sits.
//
// An identity can also repeat within one generation: claude writes a
// session's earlier entries again, verbatim, when the session resumes. The
// caller keeps one event per identity, and a repeat is a no-op — never an
// error that fails the batch's transaction, which would then fail forever.
//
// These are not the NativeIDs Read gives, which number text by its position
// in the whole file; Read keeps those for its existing callers.
type FollowedEvent struct {
	Event  Event
	Offset int64 // byte offset of the record in the file
	Block  int   // index of the content block within the record
}

// A SourceError is a complete record the decoder could not read. The batch
// that carries it moves the checkpoint past the record, so the caller records
// it with the batch's events: nothing is skipped silently.
type SourceError struct {
	Offset int64  // byte offset of the record
	Length int64  // its length, newline included
	SHA256 string // hex SHA-256 of the file's bytes [Offset, Offset+Length)
	Err    error  // why the decoder could not read it
}

func (e SourceError) Error() string {
	return fmt.Sprintf("transcript record at offset %d (%d bytes, sha256 %s): %v", e.Offset, e.Length, e.SHA256, e.Err)
}

func (e SourceError) Unwrap() error { return e.Err }

// A Batch is what one Poll read: the events of the complete records after
// From, the records the decoder could not read, and the Checkpoint that covers
// them all. Committing a batch means storing its Events, its Errors and its
// Checkpoint in one transaction; nothing moves until the caller then Acks it.
// A batch with Checkpoint == From read nothing.
type Batch struct {
	From       Checkpoint
	Events     []FollowedEvent
	Errors     []SourceError
	Checkpoint Checkpoint
}

// checkpointWindow caps the prefix and boundary a checkpoint hashes.
const checkpointWindow = 4096

// defaultMaxBatchBytes is the batch size a zero MaxBatchBytes means.
const defaultMaxBatchBytes = 4 << 20

// A Follower reads a transcript as the harness appends to it, a batch of
// complete records at a time, without moving on until the caller says the
// batch is stored:
//
//	b, err := f.Poll()     // the records after the acknowledged checkpoint
//	...                    // store b.Events, b.Errors and b.Checkpoint in one transaction
//	err = f.Ack(b)         // then move past them
//
// Until Ack, every Poll reads from the same place and gives the same events
// the same identities, so a transaction that fails is simply retried, and a
// process that dies before committing resumes from the checkpoint it last
// stored and sees the batch again. Partial trailing bytes — a record the
// harness is still writing — wait for their newline.
//
// Usage is not accounted here: a sum over batches would count an API call once
// per content block. A Follower is for one goroutine.
type Follower struct {
	// MaxBatchBytes caps the bytes of records one Poll returns. A Poll stops at
	// the last record boundary within the cap, but always returns at least one
	// record when one is complete. 0 means 4 MiB.
	MaxBatchBytes int

	path   string
	decode Decoder
	cp     Checkpoint
}

// NewFollower follows the transcript at path for sessionID, from the
// checkpoint the caller last committed, or from the start of the file given
// the zero Checkpoint. decode reads the harness's records.
//
// The file need not exist yet: a harness creates its transcript when it
// writes the first record, and Poll reports fs.ErrNotExist until then.
func NewFollower(path, sessionID string, from Checkpoint, decode Decoder) (*Follower, error) {
	switch {
	case path == "":
		return nil, errors.New("transcript follower: empty path")
	case sessionID == "":
		return nil, errors.New("transcript follower: empty session id")
	case decode == nil:
		return nil, errors.New("transcript follower: nil decoder")
	case from.Version > FollowerVersion:
		return nil, fmt.Errorf("transcript follower: checkpoint version %d is newer than this follower's %d", from.Version, FollowerVersion)
	case from.SessionID != "" && from.SessionID != sessionID:
		return nil, fmt.Errorf("transcript follower: checkpoint is for session %s, not %s", from.SessionID, sessionID)
	case from.Generation == "" && from != (Checkpoint{SessionID: from.SessionID, Version: from.Version}):
		return nil, errors.New("transcript follower: checkpoint has a position but no generation")
	case from.Offset < 0 || from.NextSeq < 0 || from.PrefixLen < 0 || from.BoundaryLen < 0 ||
		from.PrefixLen > from.Offset || from.BoundaryLen > from.Offset:
		return nil, fmt.Errorf("transcript follower: checkpoint window [%d,%d] does not fit offset %d", from.PrefixLen, from.BoundaryLen, from.Offset)
	}
	if from.Generation == "" {
		from = Checkpoint{SessionID: sessionID, Generation: sessionid.NewUUID()}
	}
	from.Version = FollowerVersion
	return &Follower{path: path, decode: decode, cp: from}, nil
}

// Path is the transcript file the follower reads.
func (f *Follower) Path() string { return f.path }

// Checkpoint is where the next Poll starts: the checkpoint of the last batch
// acknowledged, or, after a reset, the new generation at offset 0.
func (f *Follower) Checkpoint() Checkpoint { return f.cp }

// Offset is Checkpoint().Offset — a convenience; resuming needs the whole
// Checkpoint.
func (f *Follower) Offset() int64 { return f.cp.Offset }

// Ack moves the follower past b once the caller has committed it: the next
// Poll starts at b.Checkpoint. It refuses a batch that did not start where
// the follower stands — one polled before an earlier Ack or a reset.
func (f *Follower) Ack(b Batch) error {
	if b.From != f.cp {
		return fmt.Errorf("transcript follower: batch starts at generation %s offset %d, but the follower is at generation %s offset %d",
			b.From.Generation, b.From.Offset, f.cp.Generation, f.cp.Offset)
	}
	f.cp = b.Checkpoint
	return nil
}

// Poll reads the complete records after the follower's checkpoint and returns
// them as a Batch, without moving: see Follower.
//
// It first checks that the file still extends the checkpoint. When it does
// not, Poll returns a *ResetError (matching ErrTranscriptReset) with an empty
// batch, and the follower moves to a new generation at offset 0; the next Poll
// reads the file again from its start.
func (f *Follower) Poll() (Batch, error) {
	from := f.cp
	empty := Batch{From: from, Checkpoint: from}

	file, err := os.Open(f.path)
	if err != nil {
		return empty, fmt.Errorf("transcript follower: %w", err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return empty, fmt.Errorf("transcript follower: %w", err)
	}
	size, inode := info.Size(), fileInode(info)

	head, tail, reason, err := f.verify(file, size, inode)
	if err != nil {
		return empty, err
	}
	if reason != "" {
		f.cp = Checkpoint{Version: FollowerVersion, SessionID: from.SessionID, Generation: sessionid.NewUUID()}
		return empty, &ResetError{Reason: reason, Previous: from, Size: size, Inode: inode}
	}

	data, err := f.readRecords(file, from.Offset, size)
	if err != nil {
		return empty, err
	}
	if len(data) == 0 {
		return empty, nil
	}

	b := Batch{From: from}
	next := from
	offset := from.Offset
	for rest := data; len(rest) > 0; {
		n := bytes.IndexByte(rest, '\n') + 1
		record := rest[:n]
		rest = rest[n:]
		at := offset
		offset += int64(n)

		body := record[:n-1]
		if len(bytes.TrimSpace(body)) == 0 {
			continue
		}
		blocks, err := f.decode(body)
		if err != nil {
			sum := sha256.Sum256(record)
			b.Errors = append(b.Errors, SourceError{Offset: at, Length: int64(n), SHA256: hex.EncodeToString(sum[:]), Err: err})
			continue
		}
		for _, be := range blocks {
			e := be.Event
			e.Seq = next.NextSeq
			e.NativeID = followerID(e, be.Block, from.Generation, at)
			next.NextSeq++
			b.Events = append(b.Events, FollowedEvent{Event: e, Offset: at, Block: be.Block})
		}
	}

	next.Offset = offset
	if inode != 0 {
		next.Inode = inode
	}
	if from.PrefixLen == from.Offset { // the prefix is still the whole file read so far
		next.PrefixLen, next.PrefixSHA256 = window(head, data, true)
	}
	next.BoundaryLen, next.BoundarySHA256 = window(tail, data, false)
	b.Checkpoint = next
	return b, nil
}

// verify checks the file against the checkpoint, returning the checkpoint's
// prefix and boundary bytes as the file holds them now — the next checkpoint
// hashes them together with the new records — or why the file no longer
// extends the checkpoint.
func (f *Follower) verify(file *os.File, size int64, inode uint64) (head, tail []byte, reason ResetReason, err error) {
	cp := f.cp
	if cp.Offset == 0 {
		return nil, nil, "", nil // nothing read yet, nothing to contradict
	}
	if cp.Inode != 0 && inode != 0 && inode != cp.Inode {
		return nil, nil, ResetReplaced, nil
	}
	if size < cp.Offset {
		return nil, nil, ResetShrunk, nil
	}
	if head, err = readAt(file, 0, cp.PrefixLen); errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, nil, ResetShrunk, nil // it shrank since the Stat
	} else if err != nil {
		return nil, nil, "", err
	}
	if sha256Hex(head) != cp.PrefixSHA256 {
		return nil, nil, ResetPrefixChanged, nil
	}
	if tail, err = readAt(file, cp.Offset-cp.BoundaryLen, cp.BoundaryLen); errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, nil, ResetShrunk, nil
	} else if err != nil {
		return nil, nil, "", err
	}
	if sha256Hex(tail) != cp.BoundarySHA256 {
		return nil, nil, ResetBoundaryChanged, nil
	}
	return head, tail, "", nil
}

// readRecords reads the complete records from offset: up to MaxBatchBytes of
// them, but always the first one whole, however long.
func (f *Follower) readRecords(file *os.File, offset, size int64) ([]byte, error) {
	limit := int64(f.MaxBatchBytes)
	if limit <= 0 {
		limit = defaultMaxBatchBytes
	}
	var data []byte
	for pos := offset; pos < size; {
		chunk, err := readAt(file, pos, min(limit, size-pos))
		if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, err
		}
		if len(chunk) == 0 {
			break // it shrank since the Stat; the next Poll sees it
		}
		pos += int64(len(chunk))
		if data == nil {
			data = chunk
			if end := bytes.LastIndexByte(data, '\n'); end >= 0 {
				return data[:end+1], nil
			}
			continue
		}
		// One record longer than the cap: read on to its newline.
		if end := bytes.IndexByte(chunk, '\n'); end >= 0 {
			return append(data, chunk[:end+1]...), nil
		}
		data = append(data, chunk...)
	}
	return nil, nil // no newline yet: the record is still being written
}

// window hashes the checkpoint window that follows from the previous one,
// prev, and the bytes read after it, data: the first checkpointWindow bytes of
// prev+data when first is set, else the last.
func window(prev, data []byte, first bool) (int64, string) {
	n := min(len(prev)+len(data), checkpointWindow)
	h := sha256.New()
	if first {
		_, _ = h.Write(prev[:min(len(prev), n)])
		_, _ = h.Write(data[:n-min(len(prev), n)])
	} else {
		fromData := min(len(data), n)
		_, _ = h.Write(prev[len(prev)-(n-fromData):])
		_, _ = h.Write(data[len(data)-fromData:])
	}
	return int64(n), hex.EncodeToString(h.Sum(nil))
}

// followerID is the identity a Follower gives an event; see FollowedEvent.
func followerID(e Event, block int, generation string, offset int64) string {
	v := "v" + strconv.Itoa(FollowerVersion) + ":" + e.Type
	switch {
	case (e.Type == EventToolUse || e.Type == EventToolResult) && e.ToolUseID != "":
		return v + ":tool:" + e.ToolUseID
	case e.UUID != "":
		return v + ":line:" + e.UUID + ":" + strconv.Itoa(block)
	default:
		return v + ":gen:" + generation + ":" + strconv.FormatInt(offset, 10) + ":" + strconv.Itoa(block)
	}
}

// readAt reads exactly n bytes at off, or returns io.ErrUnexpectedEOF with
// what there was.
func readAt(file *os.File, off, n int64) ([]byte, error) {
	buf := make([]byte, n)
	got, err := io.ReadFull(io.NewSectionReader(file, off, n), buf)
	if errors.Is(err, io.EOF) {
		err = io.ErrUnexpectedEOF
	}
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, fmt.Errorf("transcript follower: read %s: %w", file.Name(), err)
	}
	return buf[:got], err
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

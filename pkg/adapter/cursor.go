package adapter

import (
	"context"
	"encoding/json"
	"strconv"
	"sync"
	"time"

	"github.com/olesho/harness-wrapper/pkg/contract"
)

// pollEvery is how often a waiting Observe reads the record again.
const pollEvery = 200 * time.Millisecond

// A cursor delivers observations as batches: the live ones in the order they
// were learned, and the record's one chunk at a time, in record order after
// whatever was learned before it was read.
//
// A chunk's items may span batches. Its reset, rescan and faults go with the
// batch holding its first item, and its checkpoint with the batch holding its
// last, so no checkpoint ever covers an item the Supervisor has not
// committed. The reader moves past the chunk once that batch is acknowledged;
// until then nothing more is read, and a crash replays the chunk from the
// last committed checkpoint, under the same ids.
type cursor struct {
	readerMu sync.Mutex // serializes the reader: Read and Commit
	reader   Reader

	mu        sync.Mutex
	queue     []entry
	chunk     *Chunk // read, and not yet committed
	out       *contract.Batch
	outN      int  // queue entries the outstanding batch covers
	outCommit bool // acking it commits the chunk
	lastAcked string
	seq       int
	observing bool
	notify    chan struct{}
	endOfRec  bool // a record handle: report an empty record as ended
}

// entry is one queued observation, or — obs nil — a chunk that has no items
// and only its checkpoint or faults to deliver.
type entry struct {
	obs  *contract.Observation
	size int
	meta *Chunk // on a chunk's first entry: its reset, rescan and faults
	last bool   // a chunk's last entry: its checkpoint goes with it
}

func newCursor(r Reader, endOfRecord bool) *cursor {
	return &cursor{reader: r, notify: make(chan struct{}), endOfRec: endOfRecord}
}

// push queues live observations.
func (c *cursor) push(obs ...contract.Observation) {
	c.mu.Lock()
	for i := range obs {
		o := bound(obs[i])
		c.queue = append(c.queue, entry{obs: &o, size: encodedSize(o)})
	}
	c.wakeLocked()
	c.mu.Unlock()
}

func (c *cursor) wakeLocked() {
	close(c.notify)
	c.notify = make(chan struct{})
}

// read reads the record's next chunk into the queue, unless one is pending.
// It reports whether the record had anything new.
func (c *cursor) read(ctx context.Context, max int) (bool, error) {
	c.mu.Lock()
	pending := c.chunk != nil
	c.mu.Unlock()
	if pending {
		return false, nil
	}
	c.readerMu.Lock()
	if c.reader == nil {
		c.readerMu.Unlock()
		return false, nil
	}
	ch, err := c.reader.Read(ctx, max)
	c.readerMu.Unlock()
	if err != nil {
		return false, err
	}
	if ch.empty() {
		return false, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.chunk != nil { // read concurrently: keep the first
		return false, nil
	}
	c.chunk = &ch
	if len(ch.Items) == 0 {
		c.queue = append(c.queue, entry{meta: &ch, last: true, size: metaSize(&ch)})
	}
	for i := range ch.Items {
		o := bound(ch.Items[i])
		e := entry{obs: &o, size: encodedSize(o)}
		if i == 0 {
			e.meta = &ch
			e.size += metaSize(&ch)
		}
		e.last = i == len(ch.Items)-1
		c.queue = append(c.queue, e)
	}
	c.wakeLocked()
	return true, nil
}

// observe returns the outstanding batch, or the next one, waiting up to wait
// for one to exist.
func (c *cursor) observe(ctx context.Context, wait time.Duration, maxBytes int) (contract.Batch, error) {
	if wait < 0 || wait > contract.MaxObserveWait {
		return contract.Batch{}, &contract.Error{Code: contract.CodeProtocol, Field: "wait_ms", Message: "out of range"}
	}
	if maxBytes < contract.MinObserveBytes || maxBytes > contract.MaxObserveBytes {
		return contract.Batch{}, &contract.Error{Code: contract.CodeProtocol, Field: "max_bytes", Message: "out of range"}
	}
	c.mu.Lock()
	if c.observing {
		c.mu.Unlock()
		return contract.Batch{}, contract.Errorf(contract.CodeUnexpected, "another observe is pending")
	}
	c.observing = true
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.observing = false
		c.mu.Unlock()
	}()

	deadline := time.Now().Add(wait)
	for {
		if _, err := c.read(ctx, maxBytes); err != nil {
			return contract.Batch{}, &contract.Error{Code: contract.CodeInternal, Message: "reading the record: " + err.Error()}
		}
		c.mu.Lock()
		if c.out != nil || len(c.queue) > 0 {
			b, err := c.buildLocked(maxBytes)
			c.mu.Unlock()
			return b, err
		}
		notify := c.notify
		ended := c.endOfRec && c.chunk == nil
		c.mu.Unlock()
		if ended {
			return contract.Batch{Items: []contract.Observation{}, EndOfRecord: true}, nil
		}
		left := time.Until(deadline)
		if left <= 0 {
			return contract.Batch{Items: []contract.Observation{}}, nil
		}
		t := time.NewTimer(min(left, pollEvery))
		select {
		case <-notify:
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			return contract.Batch{Items: []contract.Observation{}}, nil
		}
		t.Stop()
	}
}

// buildLocked makes the next batch from the queue's head, or replays the
// outstanding one.
func (c *cursor) buildLocked(maxBytes int) (contract.Batch, error) {
	if c.out != nil {
		return *c.out, nil
	}
	b := contract.Batch{Items: []contract.Observation{}}
	size, n, commit := batchOverhead, 0, false
	for n < len(c.queue) {
		e := c.queue[n]
		add := e.size + 1
		if e.last && c.chunk != nil && c.chunk.Checkpoint != nil {
			add += checkpointSize(c.chunk.Checkpoint)
		}
		if size+add > maxBytes {
			if n == 0 {
				return contract.Batch{}, &contract.Error{
					Code: contract.CodeBatchTooLarge, RequiredBytes: size + add,
					Message: "the next observation needs a larger bound",
				}
			}
			break
		}
		size += add
		if e.obs != nil {
			b.Items = append(b.Items, *e.obs)
		}
		if m := e.meta; m != nil {
			b.Reset, b.Rescan = m.Reset, m.Rescan
			b.Faults = append(b.Faults, m.Faults...)
		}
		if e.last {
			commit = true
			if c.chunk != nil {
				b.Checkpoint = c.chunk.Checkpoint
			}
		}
		n++
	}
	c.seq++
	b.BatchID = "b" + strconv.Itoa(c.seq)
	c.out, c.outN, c.outCommit = &b, n, commit
	return b, nil
}

// ack acknowledges the outstanding batch: its entries leave the queue, and
// the chunk whose last entry it held is committed.
func (c *cursor) ack(batchID string) error {
	c.mu.Lock()
	switch {
	case c.out != nil && c.out.BatchID == batchID:
	case batchID != "" && batchID == c.lastAcked:
		c.mu.Unlock()
		return nil
	default:
		c.mu.Unlock()
		return contract.Errorf(contract.CodeUnexpected, "batch %q is not the outstanding one", batchID)
	}
	c.queue = append(c.queue[:0:0], c.queue[c.outN:]...)
	chunk := c.chunk
	commit := c.outCommit
	c.lastAcked = batchID
	c.out, c.outN, c.outCommit = nil, 0, false
	if commit {
		c.chunk = nil
	}
	c.wakeLocked()
	c.mu.Unlock()
	if commit && chunk != nil {
		// The Supervisor has committed the chunk. If the reader cannot move
		// past it, the next read delivers it again, under the same ids.
		c.readerMu.Lock()
		_ = c.reader.Commit(*chunk)
		c.readerMu.Unlock()
	}
	return nil
}

// settled reports whether everything learned has been acknowledged and the
// record holds nothing more, reading it if need be.
func (c *cursor) settled(ctx context.Context) bool {
	c.mu.Lock()
	busy := c.out != nil || len(c.queue) > 0 || c.chunk != nil
	c.mu.Unlock()
	if busy {
		return false
	}
	more, err := c.read(ctx, contract.MaxObserveBytes)
	return err == nil && !more
}

// waitChange waits for the cursor to change, or d to pass.
func (c *cursor) waitChange(ctx context.Context, d time.Duration) {
	c.mu.Lock()
	notify := c.notify
	c.mu.Unlock()
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-notify:
	case <-t.C:
	case <-ctx.Done():
	}
}

// batchOverhead bounds a batch's bytes beyond its items and checkpoint.
const batchOverhead = 256

func encodedSize(o contract.Observation) int {
	b, err := json.Marshal(o)
	if err != nil {
		return 0
	}
	return len(b)
}

func metaSize(ch *Chunk) int {
	b, _ := json.Marshal(struct {
		Reset  *contract.Reset  `json:"reset,omitempty"`
		Rescan *contract.Rescan `json:"rescan,omitempty"`
		Faults []contract.Fault `json:"faults,omitempty"`
	}{ch.Reset, ch.Rescan, ch.Faults})
	return len(b)
}

func checkpointSize(cp *contract.Checkpoint) int {
	b, _ := json.Marshal(cp)
	return len(b)
}

// bound keeps an observation within the largest batch there is: one that
// could never be delivered keeps its identity and loses its data, marked
// truncated. Profiles bound their text fields themselves; this is the net
// under them.
func bound(o contract.Observation) contract.Observation {
	if encodedSize(o)+batchOverhead <= contract.MaxObserveBytes {
		return o
	}
	o.Data = json.RawMessage(`{}`)
	o.Truncated = true
	return o
}

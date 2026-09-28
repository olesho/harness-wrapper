package adapter

import (
	"context"
	"fmt"
	"time"

	"github.com/olesho/harness-wrapper/pkg/contract"
)

// record is a record handle: the record's items from a checkpoint, and
// Recover, with no harness process.
type record struct {
	cur     *cursor
	r       Reader
	markers *Markers
	session string
}

func newRecord(r Reader, m *Markers, sessionID string) *record {
	return &record{cur: newCursor(r, true), r: r, markers: m, session: sessionID}
}

func (r *record) Observe(ctx context.Context, wait time.Duration, maxBytes int) (contract.Batch, error) {
	return r.cur.observe(ctx, wait, maxBytes)
}

func (r *record) Ack(batchID string) error { return r.cur.ack(batchID) }

// Recover answers from the two durable facts: the input's submission marker,
// and the harness's record of it. Only the intact store's lack of a marker
// proves the input never ran.
func (r *record) Recover(ctx context.Context, inputID string) (contract.Recovered, error) {
	if !contract.ValidID(inputID) {
		return contract.Recovered{}, &contract.Error{Code: contract.CodeProtocol, Field: "input_id", Message: fmt.Sprintf("%q is not an id", inputID)}
	}
	mk, found, err := r.markers.Lookup(inputID)
	switch {
	case err != nil:
		return contract.Recovered{Outcome: contract.RecoveredUnknown}, nil
	case !found:
		return contract.Recovered{Outcome: contract.RecoveredNotFound}, nil
	case mk.SessionID != r.session:
		// Handed to another Session's harness: this record cannot say.
		return contract.Recovered{Outcome: contract.RecoveredUnknown}, nil
	}
	r.cur.readerMu.Lock()
	defer r.cur.readerMu.Unlock()
	got, err := r.r.Recover(ctx, mk)
	if err != nil {
		return contract.Recovered{}, &contract.Error{Code: contract.CodeInternal, Message: "reading the record: " + err.Error()}
	}
	if got.Outcome != contract.RecoveredUnknown && got.TurnID == "" {
		got.TurnID = TurnID(inputID)
	}
	return got, nil
}

func (r *record) Close() error {
	r.cur.readerMu.Lock()
	defer r.cur.readerMu.Unlock()
	return r.r.Close()
}

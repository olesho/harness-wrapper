package contract

import (
	"context"
	"time"
)

// Adapter is one harness, exposed through the interface. Describe and
// Provision are pure and serve the Supervisor; NewSession and OpenRecord serve
// the Host.
type Adapter interface {
	// Describe returns what the adapter offers. It performs no I/O.
	Describe() Descriptor
	// Provision renders the harness's files, argv and environment from a
	// harness-neutral Agent Spec. It is pure: the same request always gives
	// the same result, and it performs no I/O. The Supervisor writes the
	// result (see ProvisionResult).
	Provision(ProvisionRequest) (ProvisionResult, error)
	// NewSession returns a Session handle in state unopened, without any I/O.
	// The handle exists before opening starts, so an open can be watched and
	// cancelled.
	NewSession(OpenRequest) (Session, error)
	// OpenRecord opens a read-only handle on one Session's durable record. It
	// needs no credential, submits nothing, contacts no model, and works when
	// the harness binary is missing, as long as the record can be read.
	OpenRecord(ctx context.Context, req RecordRequest) (Record, error)
}

// Session is one harness conversation. Its methods are safe to call
// concurrently: State, Observe and Ack are never blocked by an outstanding
// Send, and Interrupt and Answer are ordered after its result.
type Session interface {
	// Open starts the Session fresh or reopens it: unopened → starting →
	// idle, blocked or exited. Cancelling ctx abandons the open; the Host then
	// calls Close.
	Open(ctx context.Context) (OpenResult, error)
	// Send submits one input. An error is an *Error carrying a Certainty.
	Send(ctx context.Context, in Input) (SendResult, error)
	// Interrupt stops the named input's turn, only if it is still the current
	// one.
	Interrupt(ctx context.Context, req InterruptRequest) (InterruptOutcome, error)
	// Answer resolves a prompt the harness raised (capability prompts).
	Answer(ctx context.Context, promptID string, c Choice) error
	// State is the current snapshot. It is observational: nothing may be
	// authorized from it.
	State() State
	// Observe returns the oldest unacknowledged batch, waiting up to wait for
	// one to exist.
	Observe(ctx context.Context, wait time.Duration, maxBytes int) (Batch, error)
	// Ack acknowledges a batch the Supervisor has committed.
	Ack(batchID string) error
	// Close terminates the harness's process group and reports whether it
	// stopped and whether every final batch was acknowledged. Observe and Ack
	// keep working while it runs. It is terminal for the handle.
	Close(ctx context.Context, reason CloseReason, drain time.Duration) (CloseResult, error)
}

// Record is a read-only handle on one Session's durable record.
type Record interface {
	// Observe delivers only record-origin items, from the checkpoint to the
	// record's current end; then an empty batch with EndOfRecord set.
	Observe(ctx context.Context, wait time.Duration, maxBytes int) (Batch, error)
	// Ack acknowledges a batch the Supervisor has committed.
	Ack(batchID string) error
	// Recover reports what became of an input, from the adapter's submission
	// marker and the harness's record.
	Recover(ctx context.Context, inputID string) (Recovered, error)
	// Close releases the handle and stops any helper it started.
	Close() error
}

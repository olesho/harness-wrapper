package contract

import (
	"context"
	"time"
)

// Adapter is one harness, exposed through the interface. Describe, Provision
// and Placeholder are pure and serve the Supervisor; NewSession and OpenRecord
// serve the Host; Keep serves the runtime's login keeper.
type Adapter interface {
	// Describe returns what the adapter offers. It performs no I/O.
	Describe() Descriptor
	// Provision renders the harness's files, argv and environment from a
	// harness-neutral Agent Spec. It is pure: the same request always gives
	// the same result, and it performs no I/O. The Supervisor writes the
	// result (see ProvisionResult). A request that loads a saved Session
	// (ProvisionRequest.Load) is also told where that Session's history goes.
	Provision(ProvisionRequest) (ProvisionResult, error)
	// Placeholder renders what stands in for a credential (capability
	// brokered_credentials): the file the Supervisor stages in its place,
	// and the substitutions an egress broker makes for it. It is pure. Its
	// result holds the credential's secrets: the Supervisor hands them to
	// the broker alone, and never journals or logs them. Without the
	// capability, or for a kind the Descriptor's Egress does not route, it
	// answers CodeUnsupported.
	Placeholder(PlaceholderRequest) (PlaceholderResult, error)
	// Keep opens a keeper of the harness's subscription login under a home
	// of the runtime's keeper identity (capability login_keeper). It starts
	// nothing until a keeper method needs the harness. Without the
	// capability it answers CodeUnsupported.
	Keep(KeeperRequest) (Keeper, error)
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
	// idle, blocked or exited. Cancelling ctx abandons the open: Open fails
	// CodeOpenFailed with no Reason (OpenAbandoned), since the caller, which
	// owns ctx, knows whether it cancelled or timed out; the Host then calls
	// Close.
	Open(ctx context.Context) (OpenResult, error)
	// Send submits one input. An error is an *Error carrying a Certainty. A
	// turn the harness started itself (capability autonomous_turns) is
	// stopped first: the input's turn is the input's alone.
	Send(ctx context.Context, in Input) (SendResult, error)
	// Interrupt stops the named turn — an input's, or one the harness
	// started itself — only if it is still the current one.
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
	// record's current end; then an empty batch with EndOfRecord set. Read
	// so with no checkpoint, a loaded Session's record gives the checkpoint
	// its new history starts from; a record that reads empty is not where
	// the harness looks.
	Observe(ctx context.Context, wait time.Duration, maxBytes int) (Batch, error)
	// Ack acknowledges a batch the Supervisor has committed.
	Ack(batchID string) error
	// Recover reports what became of an input, from the adapter's submission
	// marker and the harness's record.
	Recover(ctx context.Context, inputID string) (Recovered, error)
	// Close releases the handle and stops any helper it started.
	Close() error
}

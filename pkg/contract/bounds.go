package contract

import "time"

// Bounds and deadlines. They are initial values; each is part of the
// contract.
const (
	// MaxInputBytes bounds one Send's content; a Descriptor may lower it.
	MaxInputBytes = 1 << 20
	// MinObserveBytes and MaxObserveBytes bound Observe's maxBytes.
	MinObserveBytes = 64 << 10
	MaxObserveBytes = 16 << 20
	// MaxObservationText bounds one text field of an observation; a longer
	// one is cut, and the observation marked truncated.
	MaxObservationText = 64 << 10
	// MaxOpenConfigBytes and MaxCheckpointBytes bound those opaque values.
	MaxOpenConfigBytes = 64 << 10
	MaxCheckpointBytes = 64 << 10
	// MaxMessageBytes bounds an error's message.
	MaxMessageBytes = 4 << 10
	// MaxObserveWait bounds Observe's wait.
	MaxObserveWait = 30 * time.Second
)

// Deadlines: how long the Host waits for each operation. A deadline ends the
// Host's wait, not the operation, which may still take effect.
const (
	OpenDeadline    = 120 * time.Second
	SendDeadline    = 120 * time.Second
	AnswerDeadline  = 5 * time.Second
	StateDeadline   = 5 * time.Second
	AckDeadline     = 5 * time.Second
	RecoverDeadline = 30 * time.Second
	// ObserveSlack and CloseSlack are added to the operation's own wait and
	// drain.
	ObserveSlack = 5 * time.Second
	CloseSlack   = 5 * time.Second
	// DefaultInterruptDeadline, MinInterruptDeadline and MaxInterruptDeadline
	// bound InterruptRequest.DeadlineMS.
	DefaultInterruptDeadline = 10 * time.Second
	MinInterruptDeadline     = 100 * time.Millisecond
	MaxInterruptDeadline     = 60 * time.Second
	// DefaultDrain and MaxDrain bound Close's drain.
	DefaultDrain = 10 * time.Second
	MaxDrain     = 60 * time.Second
)

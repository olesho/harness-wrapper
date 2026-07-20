# Screen (vt100)

`pkg/screen` is layer 2: it turns the harness's raw PTY byte stream — ANSI escapes and all — into a
queryable terminal state. Turn detection needs the *rendered* screen (clean text), not the byte soup,
so the adapters read `Snapshot`s, not raw bytes.

It wraps exactly one vt100 emulator, `github.com/hinshun/vt10x`, chosen by measurement in
[ADR-001](decisions/adr-001-vt100.md).

## API

```go
func New(cols, rows int) *Screen                     // defaults to 120×40 if non-positive

func (s *Screen) Write(p []byte) (int, error)        // feed raw PTY bytes; bumps Generation, signals subscribers
func (s *Screen) Snapshot() Snapshot                 // coherent point-in-time view
func (s *Screen) Generation() uint64                 // cheap "has anything changed?" counter
func (s *Screen) Resize(cols, rows int)              // update dimensions; bumps Generation
func (s *Screen) ResizeWithPeer(cols, rows int, resizePeer func() error) error // coordinate a fallible peer resize without freezing readers
func (s *Screen) Subscribe() (<-chan struct{}, func()) // change notifications + an unsubscribe func
```

All methods are concurrent-safe.

```go
type Snapshot struct {
	Text       string // rendered screen, top-to-bottom, '\n' per row
	Cols, Rows int
	CursorCol  int    // 0-indexed
	CursorRow  int    // 0-indexed
	Generation uint64 // monotonic; increments on each Write or size change
}
```

## How it's used

- The [wrapper](wrapper.md) supervisor's `Stdout` is pointed at a `Screen` by
  [`chat.Open`](../guide/chat.md#open), so every PTY byte the harness emits is rendered.
- `Subscribe()` drives the [turns Watcher](turns.md#the-watcher)'s screen pump: every successful
  `Write` or size change signals (coalesced, buffer of 1), the Watcher takes a `Snapshot`, and hands
  it to `adapter.OnScreen`.
- `Generation` lets callers cheaply detect change without rendering a full snapshot.
- `ResizeWithPeer` runs a fallible peer operation (normally a PTY resize) WITHOUT holding the screen
  read/write lock, then commits the emulator dimensions under a brief lock only when that operation
  succeeds. Not holding the lock across the peer keeps `Write`/`Snapshot` live even when the peer
  resize blocks (a PTY window-size ioctl can stall in the kernel behind an in-flight stdin write);
  concurrent resizes are serialized separately so the peer op and its commit never interleave.

The emulator wrapping is intentionally thin — `Write` / `Snapshot` / `Resize` / `ResizeWithPeer` /
`Subscribe` is the whole surface. If a harness's rendering ever exceeds the emulator's fidelity, the
fix is to bias that [adapter](turns.md) toward the harness's own [transcript](transcript.md) for text and
use the screen only for the liveness signal — exactly the consequence ADR-001 anticipated.

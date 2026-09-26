package wrapper

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

// panickingClassifier stands in for a Classifier with a bug: the caller's own,
// or a built-in pattern set such as the Codex matcher that sliced past the end
// of its input. Before the recover, its panic ran on the classifier goroutine
// and ended the host process, whatever the caller did.
var panickingClassifier = ClassifierFunc(func(ClassifierInput) Classification {
	panic("classifier bug")
})

// waitWithin returns what Wait returns, failing the test when it does not
// return within d.
func waitWithin(t *testing.T, s *Session, d time.Duration) (Result, error) {
	t.Helper()
	type waited struct {
		res Result
		err error
	}
	done := make(chan waited, 1)
	go func() {
		res, err := s.Wait()
		done <- waited{res, err}
	}()
	select {
	case w := <-done:
		return w.res, w.err
	case <-time.After(d):
		t.Fatalf("Wait did not return within %s", d)
		return Result{}, nil
	}
}

// A Classifier that panics while the harness runs ends the run: the harness
// is terminated, and Wait reports the failure — as its doc always said it
// would — instead of the process dying with it.
func TestClassifierPanicEndsTheRun(t *testing.T) {
	for _, keepAlive := range []bool{false, true} {
		t.Run(fmt.Sprintf("KeepAliveOnClassification=%v", keepAlive), func(t *testing.T) {
			var log traceLog
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			s, err := Start(ctx, Config{
				BinaryPath:                "/bin/sh",
				Args:                      []string{"-c", "echo working; exec sleep 30"},
				Stdout:                    io.Discard,
				Classifier:                panickingClassifier,
				IdleQuiet:                 50 * time.Millisecond,
				IdleClassify:              200 * time.Millisecond,
				WaitDelay:                 time.Second,
				KeepAliveOnClassification: keepAlive,
				Trace:                     &log,
			})
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			res, err := waitWithin(t, s, 10*time.Second)
			if !errors.Is(err, ErrClassifierPanic) {
				t.Fatalf("Wait err = %v, want ErrClassifierPanic", err)
			}
			if !strings.Contains(err.Error(), "classifier bug") {
				t.Errorf("Wait err = %q, want it to carry the panic value", err)
			}
			if res.Status != StatusUnknown {
				t.Errorf("Status = %q, want %q: the wrapper stopped the harness because it could no longer watch it", res.Status, StatusUnknown)
			}
			if !strings.Contains(res.Reason, "supervision failed") || !strings.Contains(res.Reason, "classifier bug") {
				t.Errorf("Reason = %q, want it to name the supervision failure", res.Reason)
			}
			if stack, _ := log.fields(t, "classifier_panic")["stack"].(string); !strings.Contains(stack, "panickingClassifier") && !strings.Contains(stack, "supervision_failure_test.go") {
				t.Errorf("classifier_panic stack does not show the panicking classifier:\n%s", stack)
			}
			// Every call returns the same value.
			if res2, err2 := s.Wait(); res2 != res || !errors.Is(err2, ErrClassifierPanic) {
				t.Errorf("second Wait = (%+v, %v), want (%+v, %v)", res2, err2, res, err)
			}
		})
	}
}

// A Classifier that panics in the exit pass, after the harness exited on its
// own, is reported by Wait too; the Result still describes that exit.
func TestClassifierPanicOnExitPassKeepsTheExit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	// No output, so no mid-run pass runs; the failed exit runs the exit pass.
	s, err := Start(ctx, Config{
		BinaryPath: "/bin/sh",
		Args:       []string{"-c", "exit 3"},
		Stdout:     io.Discard,
		Classifier: panickingClassifier,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	res, err := waitWithin(t, s, 10*time.Second)
	if !errors.Is(err, ErrClassifierPanic) {
		t.Fatalf("Wait err = %v, want ErrClassifierPanic", err)
	}
	if res.Status != StatusFailed || res.ExitCode != 3 {
		t.Errorf("Result = %q exit %d, want %q exit 3: the harness's own exit is still what the Result describes", res.Status, res.ExitCode, StatusFailed)
	}
}

// errorReader passes the first read through and then fails, standing in for a
// PTY master whose read breaks while the harness still runs.
type errorReader struct {
	r    io.Reader
	err  error
	read bool
}

func (e *errorReader) Read(p []byte) (int, error) {
	if e.read {
		return 0, e.err
	}
	e.read = true
	return e.r.Read(p)
}

var errReadBroke = errors.New("read broke")

// A read of the harness's output that fails ends the run and is reported as
// ErrPTYRead. Before, the read loop took any error for the end of the output
// and returned: the harness ran on unread, and Wait returned nil.
func TestPTYReadFailureEndsTheRun(t *testing.T) {
	orig := ptyOutputReader
	ptyOutputReader = func(ptmx io.Reader) io.Reader { return &errorReader{r: ptmx, err: errReadBroke} }
	defer func() { ptyOutputReader = orig }()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	s, err := Start(ctx, Config{
		BinaryPath: "/bin/sh",
		Args:       []string{"-c", "echo working; exec sleep 30"},
		Stdout:     io.Discard,
		WaitDelay:  time.Second,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	res, err := waitWithin(t, s, 10*time.Second)
	if !errors.Is(err, ErrPTYRead) || !errors.Is(err, errReadBroke) {
		t.Fatalf("Wait err = %v, want ErrPTYRead wrapping the read's error", err)
	}
	if res.Status != StatusUnknown {
		t.Errorf("Status = %q, want %q", res.Status, StatusUnknown)
	}
}

// outputEnded separates the ends of the output — what each platform's master
// read reports once nothing holds the terminal, and the supervisor's own
// close — from failures to read it. A wrong end here turns every run into a
// Wait error.
func TestOutputEnded(t *testing.T) {
	ends := []error{
		io.EOF, // macOS; a contained session's stopped reader
		&fs.PathError{Op: "read", Path: "/dev/ptmx", Err: syscall.EIO}, // Linux
		syscall.EIO, // a contained session's raw read
		&fs.PathError{Op: "read", Path: "/dev/ptmx", Err: os.ErrClosed}, // closed by the supervisor
	}
	for _, err := range ends {
		if !outputEnded(err) {
			t.Errorf("outputEnded(%v) = false, want true", err)
		}
	}
	for _, err := range []error{errReadBroke, syscall.EBADF, &fs.PathError{Op: "read", Path: "/dev/ptmx", Err: syscall.ENXIO}} {
		if outputEnded(err) {
			t.Errorf("outputEnded(%v) = true, want false", err)
		}
	}
}

// closedMasterReader reads nothing until the supervisor closes the master, and
// then reports it the way a contained session's reader does: through RawConn,
// whose error for a closed descriptor is Go's internal "use of closed file",
// which is neither os.ErrClosed nor EOF.
type closedMasterReader struct{ master *os.File }

var errUseOfClosedFile = errors.New("use of closed file")

func (c closedMasterReader) Read([]byte) (int, error) {
	for {
		if _, err := c.master.Stat(); errors.Is(err, os.ErrClosed) {
			return 0, errUseOfClosedFile
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A read the supervisor ended itself — the master closed after the drain
// budget — ends the output whatever error it reports; it is not a failure.
func TestReadEndedByTheSupervisorIsNotAFailure(t *testing.T) {
	orig := ptyOutputReader
	ptyOutputReader = func(ptmx io.Reader) io.Reader { return closedMasterReader{master: ptmx.(*os.File)} }
	defer func() { ptyOutputReader = orig }()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	res, err := Run(ctx, Config{BinaryPath: "/bin/sh", Args: []string{"-c", "exit 0"}, Stdout: io.Discard})
	if err != nil {
		t.Fatalf("Run err = %v, want nil: the supervisor closed the master itself", err)
	}
	if res.Status != StatusIdle {
		t.Errorf("Status = %q, want %q", res.Status, StatusIdle)
	}
}

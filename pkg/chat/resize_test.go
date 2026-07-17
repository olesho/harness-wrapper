package chat

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/pkg/wrapper"
	"golang.org/x/term"
)

const resizeHelperEnv = "HARNESS_WRAPPER_RESIZE_HELPER"

// TestConversationResizeHelperProcess is re-executed under a PTY by the
// resize tests. Each input line asks it to report the size visible from the
// child side, which verifies the kernel PTY rather than only chat's model.
func TestConversationResizeHelperProcess(t *testing.T) {
	if os.Getenv(resizeHelperEnv) != "1" {
		return
	}

	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		token := strings.TrimSpace(scanner.Text())
		if token == "quit" {
			os.Exit(0)
		}
		cols, rows, err := term.GetSize(int(os.Stdin.Fd()))
		if err != nil {
			fmt.Fprintf(os.Stdout, "SIZE_ERROR %s %v\n", token, err)
			os.Exit(2)
		}
		fmt.Fprintf(os.Stdout, "SIZE %s %d %d\n", token, cols, rows)
	}
	os.Exit(0)
}

func TestConversationResizeSynchronizesPTYAndScreen(t *testing.T) {
	conv, output := openResizeTestConversation(t)

	for i, size := range []struct {
		cols uint16
		rows uint16
	}{
		{cols: 86, rows: 51},
		{cols: 211, rows: 51},
		{cols: 144, rows: 101},
		{cols: 144, rows: 85},
	} {
		if err := conv.Resize(size.cols, size.rows); err != nil {
			t.Fatalf("Resize(%d, %d): %v", size.cols, size.rows, err)
		}

		snap := conv.screen.Snapshot()
		if snap.Cols != int(size.cols) || snap.Rows != int(size.rows) {
			t.Fatalf("screen size after Resize(%d, %d) = %dx%d", size.cols, size.rows, snap.Cols, snap.Rows)
		}

		token := fmt.Sprintf("sequential-%d", i)
		writeResizeQuery(t, conv, token)
		cols, rows := waitForReportedSize(t, output, token)
		if cols != int(size.cols) || rows != int(size.rows) {
			t.Fatalf("child PTY size after Resize(%d, %d) = %dx%d", size.cols, size.rows, cols, rows)
		}
	}
}

func TestConversationConcurrentResizeKeepsPTYAndScreenTogether(t *testing.T) {
	conv, output := openResizeTestConversation(t)

	for round := 0; round < 10; round++ {
		const workers = 32
		start := make(chan struct{})
		errs := make(chan error, workers)
		var wg sync.WaitGroup
		for i := 0; i < workers; i++ {
			cols := uint16(80 + i + round)
			rows := uint16(24 + (workers - i) + round)
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				errs <- conv.Resize(cols, rows)
			}()
		}
		close(start)
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatalf("concurrent Resize: %v", err)
			}
		}

		snap := conv.screen.Snapshot()
		token := fmt.Sprintf("concurrent-%d", round)
		writeResizeQuery(t, conv, token)
		cols, rows := waitForReportedSize(t, output, token)
		if snap.Cols != cols || snap.Rows != rows {
			t.Fatalf("round %d: screen=%dx%d child PTY=%dx%d", round, snap.Cols, snap.Rows, cols, rows)
		}
	}
}

func TestConversationResizeAfterCloseLeavesScreenUnchanged(t *testing.T) {
	conv, _ := openResizeTestConversation(t)
	before := conv.screen.Snapshot()

	if err := conv.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := conv.Resize(200, 70); !errors.Is(err, ErrClosed) {
		t.Fatalf("Resize after Close = %v, want ErrClosed", err)
	}

	after := conv.screen.Snapshot()
	if after.Cols != before.Cols || after.Rows != before.Rows {
		t.Fatalf("screen changed after rejected resize: before=%dx%d after=%dx%d", before.Cols, before.Rows, after.Cols, after.Rows)
	}
}

func TestConversationResizeAfterPTYExitLeavesScreenUnchanged(t *testing.T) {
	conv, _ := openResizeTestConversation(t)
	if _, err := conv.Wrapper().WriteStdin([]byte("quit\n")); err != nil {
		t.Fatalf("WriteStdin(quit): %v", err)
	}
	if _, err := conv.Wrapper().Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	before := conv.screen.Snapshot()

	if err := conv.Resize(200, 70); !errors.Is(err, wrapper.ErrSessionTerminated) {
		t.Fatalf("Resize after PTY exit = %v, want ErrSessionTerminated", err)
	}
	after := conv.screen.Snapshot()
	if after.Cols != before.Cols || after.Rows != before.Rows {
		t.Fatalf("screen changed after failed PTY resize: before=%dx%d after=%dx%d", before.Cols, before.Rows, after.Cols, after.Rows)
	}
}

func TestConversationSameSizeResizeIsNoOp(t *testing.T) {
	conv, _ := openResizeTestConversation(t)
	if err := conv.Resize(132, 57); err != nil {
		t.Fatalf("initial Resize: %v", err)
	}
	before := conv.screen.Snapshot()

	if err := conv.Resize(132, 57); err != nil {
		t.Fatalf("same-size Resize: %v", err)
	}
	after := conv.screen.Snapshot()
	if after.Generation != before.Generation {
		t.Fatalf("same-size resize changed screen generation: before=%d after=%d", before.Generation, after.Generation)
	}
}

func openResizeTestConversation(t *testing.T) (*Conversation, *lockedBuffer) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	conv, err := Open(ctx, Options{
		Harness:    "generic",
		BinaryPath: os.Args[0],
		Args:       []string{"-test.run=^TestConversationResizeHelperProcess$"},
		Env:        append(os.Environ(), resizeHelperEnv+"=1"),
		Cols:       80,
		Rows:       24,
		Store:      newFakeStore(),
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = conv.Close(context.Background()) })

	output := &lockedBuffer{}
	detach := conv.Wrapper().AttachOutput(output)
	t.Cleanup(detach)
	return conv, output
}

func writeResizeQuery(t *testing.T, conv *Conversation, token string) {
	t.Helper()
	if _, err := conv.Wrapper().WriteStdin([]byte(token + "\n")); err != nil {
		t.Fatalf("WriteStdin(%q): %v", token, err)
	}
}

func waitForReportedSize(t *testing.T, output *lockedBuffer, token string) (int, int) {
	t.Helper()
	pattern := regexp.MustCompile(`SIZE ` + regexp.QuoteMeta(token) + ` (\d+) (\d+)`)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if match := pattern.FindStringSubmatch(output.String()); match != nil {
			cols, err := strconv.Atoi(match[1])
			if err != nil {
				t.Fatalf("parse cols %q: %v", match[1], err)
			}
			rows, err := strconv.Atoi(match[2])
			if err != nil {
				t.Fatalf("parse rows %q: %v", match[2], err)
			}
			return cols, rows
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for size token %q; output=%q", token, output.String())
	return 0, 0
}

type lockedBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

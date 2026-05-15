// screenbench-record runs an interactive harness under wrapper
// supervision while capturing the raw PTY byte stream to a corpus
// scenario directory. Use it to build the bake-off corpus:
//
//	screenbench-record \
//	  --harness codex \
//	  --bin /usr/local/bin/codex \
//	  --out test/corpus/codex/short-reply \
//	  --cols 120 --rows 40 \
//	  --notes "single-turn short reply"
//
// After the harness exits, the user is expected to populate
// expected.txt by hand (or copy from the harness's session JSONL).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/olesho/harness-wrapper/internal/screenbench/scenario"
	"github.com/olesho/harness-wrapper/pkg/wrapper"
)

func main() {
	var (
		harness = flag.String("harness", "", "harness name (e.g. codex, claude-code); recorded in meta.json")
		bin     = flag.String("bin", "", "path to harness binary (required)")
		out     = flag.String("out", "", "output scenario directory (required)")
		cols    = flag.Int("cols", 120, "terminal columns (also passed via COLUMNS env)")
		rows    = flag.Int("rows", 40, "terminal rows (also passed via LINES env)")
		version = flag.String("binary-version", "", "harness version string for meta.json")
		notes   = flag.String("notes", "", "free-text notes for meta.json")
	)
	flag.Parse()
	if *bin == "" || *out == "" || *harness == "" {
		fmt.Fprintln(os.Stderr, "usage: screenbench-record --harness NAME --bin PATH --out DIR [--cols N] [--rows N] [--notes ...] -- [harness args]")
		os.Exit(2)
	}

	if err := os.MkdirAll(*out, 0o755); err != nil {
		fail("mkdir out: %v", err)
	}
	rawPath := filepath.Join(*out, "bytes.raw")
	rawFile, err := os.Create(rawPath)
	if err != nil {
		fail("create bytes.raw: %v", err)
	}
	defer rawFile.Close()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	env := append(os.Environ(),
		fmt.Sprintf("COLUMNS=%d", *cols),
		fmt.Sprintf("LINES=%d", *rows),
	)

	cfg := wrapper.Config{
		BinaryPath: *bin,
		Args:       flag.Args(),
		Env:        env,
		Stdin:      os.Stdin,
		Stdout:     os.Stdout,
	}

	sess, err := wrapper.Start(ctx, cfg)
	if err != nil {
		fail("wrapper start: %v", err)
	}
	detach := sess.AttachOutput(rawFile)

	res, _ := sess.Wait()
	detach()

	if err := scenario.WriteMeta(*out, scenario.Meta{
		Harness:       *harness,
		BinaryVersion: *version,
		RecordedAt:    time.Now().UTC(),
		Cols:          *cols,
		Rows:          *rows,
		Notes:         *notes,
	}); err != nil {
		fail("write meta: %v", err)
	}

	fmt.Fprintf(os.Stderr, "\n[screenbench-record] captured %d bytes to %s (status=%s, exit=%d)\n",
		fileSize(rawPath), rawPath, res.Status, res.ExitCode)
	fmt.Fprintf(os.Stderr, "[screenbench-record] populate %s with the ground-truth final assistant text\n",
		filepath.Join(*out, "expected.txt"))
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "screenbench-record: "+format+"\n", args...)
	os.Exit(1)
}

func fileSize(p string) int64 {
	info, err := os.Stat(p)
	if err != nil {
		return 0
	}
	return info.Size()
}

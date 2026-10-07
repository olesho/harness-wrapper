package proc

import (
	"bufio"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/pkg/contract"
)

func TestReadBoundedLine(t *testing.T) {
	const max = 8
	in := "short\r\n" + strings.Repeat("x", max) + "\r\n" + strings.Repeat("y", max+1) + "\n" +
		strings.Repeat("z", 100) + "\nnext\nlast"
	// A small buffer makes a long line span several reads.
	r := bufio.NewReaderSize(strings.NewReader(in), 16)
	var got []string
	for {
		l, err := ReadBoundedLine(r, max)
		got = append(got, string(l))
		if err != nil {
			if !errors.Is(err, io.EOF) {
				t.Fatal(err)
			}
			break
		}
	}
	want := []string{"short", strings.Repeat("x", max), "", "", "next", "last"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("lines %q, want %q", got, want)
	}
}

func TestTailBuffer(t *testing.T) {
	b := NewTailBuffer(4)
	_, _ = b.Write([]byte("abc"))
	_, _ = b.Write([]byte("def"))
	if b.String() != "cdef" {
		t.Errorf("tail %q", b.String())
	}
}

func TestLastLine(t *testing.T) {
	if got := LastLine("one\n  two  \n\n"); got != "two" {
		t.Errorf("last line %q", got)
	}
	if got := LastLine("a\n" + strings.Repeat("b", 2*DetailMax)); len(got) != DetailMax {
		t.Errorf("a long line kept %d bytes, want %d", len(got), DetailMax)
	}
}

func TestReadToken(t *testing.T) {
	dir := t.TempDir()
	write := func(name, v string) string {
		f := filepath.Join(dir, name)
		if err := os.WriteFile(f, []byte(v), 0o600); err != nil {
			t.Fatal(err)
		}
		return f
	}
	if tok, err := ReadToken(write("ok", " tok\n")); err != nil || tok != "tok" {
		t.Errorf("token %q, %v", tok, err)
	}
	for _, v := range []string{"", "a\nb", "a\rb"} {
		if _, err := ReadToken(write("bad", v)); err == nil {
			t.Errorf("%q passed", v)
		}
	}
	if _, err := ReadToken(filepath.Join(dir, "missing")); err == nil {
		t.Error("a missing file passed")
	}
}

// run starts script with /bin/sh and waits for it, returning its exit.
func run(t *testing.T, script string, stopping, killed bool) contract.SessionExitedData {
	t.Helper()
	tail := NewTailBuffer(1 << 10)
	cmd, stdin, stdout, err := Start("/bin/sh", []string{"-c", script}, t.TempDir(), nil, tail)
	if err != nil {
		t.Fatal(err)
	}
	_ = stdin.Close()
	out, _ := io.ReadAll(stdout)
	_ = stdout.Close()
	werr := cmd.Wait()
	// stderr's copy ends once its pipe does; give it a moment.
	for deadline := time.Now().Add(2 * time.Second); strings.Contains(script, "echo why") && !strings.Contains(tail.String(), "why") && time.Now().Before(deadline); {
		time.Sleep(5 * time.Millisecond)
	}
	if strings.Contains(script, "echo out") && string(out) != "out\n" {
		t.Errorf("stdout %q", out)
	}
	return Exit(cmd, werr, stopping, killed, tail.String())
}

func TestExit(t *testing.T) {
	if e := run(t, "echo out", false, false); e.Class != contract.ExitClean || e.ExitCode == nil || *e.ExitCode != 0 {
		t.Errorf("exit 0: %+v", e)
	}
	e := run(t, "echo first >&2; echo why >&2; exit 3", false, false)
	if e.Class != contract.ExitCrashed || e.ExitCode == nil || *e.ExitCode != 3 || e.Detail != "why" {
		t.Errorf("exit 3: %+v", e)
	}
	if e := run(t, "exit 1", true, false); e.Class != contract.ExitClean {
		t.Errorf("exit 1 while stopping: %+v", e)
	}
	if e := run(t, "kill -9 $$", false, false); e.Class != contract.ExitKilled || e.Signal == "" || e.ExitCode != nil {
		t.Errorf("SIGKILL: %+v", e)
	}
	if e := run(t, "kill -9 $$", true, true); e.Class != contract.ExitKilled {
		t.Errorf("SIGKILL while stopping: %+v", e)
	}
}

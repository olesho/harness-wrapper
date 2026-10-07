// Package proc holds what the profiles' transports share of running a
// harness's process: starting it on pipes in a group of its own, reading its
// lines, keeping its stderr's tail, reading its credential, and reading its
// exit as the Session's.
package proc

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/olesho/harness-wrapper/internal/procgroup"
	"github.com/olesho/harness-wrapper/pkg/contract"
)

// DetailMax bounds an exit's detail: LastLine cuts to it.
const DetailMax = 512

// Start launches bin in a process group of its own: stdin a pipe, stdout and
// stderr plain pipes read here, so Wait never waits on a descendant that
// inherited them. stderr is copied into errTo until the process's last
// writer closes it.
func Start(bin string, args []string, dir string, env []string, errTo io.Writer) (*exec.Cmd, io.WriteCloser, *os.File, error) {
	cmd := exec.Command(bin, args...)
	cmd.Dir, cmd.Env = dir, env
	procgroup.Set(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, nil, err
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		return nil, nil, nil, err
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		_, _ = outR.Close(), outW.Close()
		return nil, nil, nil, err
	}
	cmd.Stdout, cmd.Stderr = outW, errW
	if err := cmd.Start(); err != nil {
		_, _, _, _ = outR.Close(), outW.Close(), errR.Close(), errW.Close()
		return nil, nil, nil, err
	}
	_, _ = outW.Close(), errW.Close()
	go func() {
		_, _ = io.Copy(errTo, errR)
		_ = errR.Close()
	}()
	return cmd, stdin, outR, nil
}

// Exit is the Session's exit for cmd, which Wait returned waitErr for:
// clean when it exited 0 or was stopped, killed when a signal ended it (or
// SIGKILL a stop), crashed otherwise. Its detail is stderr's last line, or
// waitErr's when that is empty.
func Exit(cmd *exec.Cmd, waitErr error, stopping, killed bool, stderr string) contract.SessionExitedData {
	exit := contract.SessionExitedData{Class: contract.ExitCrashed}
	var code int
	sig := ""
	if ps := cmd.ProcessState; ps != nil {
		code, sig = ps.ExitCode(), procgroup.ExitSignal(ps)
	}
	if sig == "" && code >= 0 {
		c := code
		exit.ExitCode = &c
	}
	exit.Signal = sig
	switch {
	case stopping && sig == "killed":
		exit.Class = contract.ExitKilled
	case stopping, sig == "" && code == 0:
		exit.Class = contract.ExitClean
	case sig != "" || killed:
		exit.Class = contract.ExitKilled
	}
	exit.Detail = LastLine(stderr)
	if exit.Detail == "" && waitErr != nil && sig == "" && code != 0 {
		exit.Detail = waitErr.Error()
	}
	return exit
}

// ReadBoundedLine returns the next line without its line ending; a line
// longer than max bytes, its ending not counted, is consumed and dropped
// (nil, nil). At the end of input it returns what is left with the error.
func ReadBoundedLine(r *bufio.Reader, max int) ([]byte, error) {
	var buf []byte
	over := false
	for {
		chunk, err := r.ReadSlice('\n')
		if !over {
			// max+2 leaves room for a CRLF, judged once the line is whole.
			if len(buf)+len(chunk) > max+2 {
				over, buf = true, nil
			} else {
				buf = append(buf, chunk...)
			}
		}
		switch {
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case over:
			return nil, err
		}
		if err == nil {
			buf = bytes.TrimRight(buf, "\r\n")
		}
		if len(buf) > max {
			return nil, err
		}
		return buf, err
	}
}

// ReadToken reads a credential file: one line, no control characters.
func ReadToken(file string) (string, error) {
	b, err := os.ReadFile(file) //nolint:gosec // the staged credential's path
	if err != nil {
		var pe *os.PathError
		if errors.As(err, &pe) {
			return "", fmt.Errorf("credential file: %w", pe.Err)
		}
		return "", errors.New("credential file unreadable")
	}
	tok := strings.TrimSpace(string(b))
	if tok == "" || strings.ContainsAny(tok, "\n\r\x00") {
		return "", errors.New("credential file is empty or malformed")
	}
	return tok, nil
}

// TailBuffer keeps the last n bytes written to it.
type TailBuffer struct {
	mu  sync.Mutex
	n   int
	buf []byte
}

// NewTailBuffer is a TailBuffer of n bytes.
func NewTailBuffer(n int) *TailBuffer { return &TailBuffer{n: n} }

func (b *TailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	if over := len(b.buf) - b.n; over > 0 {
		b.buf = append(b.buf[:0], b.buf[over:]...)
	}
	return len(p), nil
}

func (b *TailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}

// LastLine is s's last nonempty line, cut to DetailMax bytes.
func LastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	l := strings.TrimSpace(lines[len(lines)-1])
	if len(l) > DetailMax {
		l = l[:DetailMax]
	}
	return l
}

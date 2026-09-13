//go:build screenbench

package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// startupFake models how claude starts, for script tests: either straight at
// the composer (a directory claude already trusts) or at the folder-trust
// dialog, in a given option order, with the highlight on the first row. It
// repaints through the driver's own output path in response to the keys a
// script sends, and records every write with the state it arrived in.
type startupFake struct {
	d *scriptDriver

	mu       sync.Mutex
	dialog   bool
	options  []string // top to bottom
	marker   int
	accepted string // the label accepted with a confirm key, "" if none
	writes   []fakeWrite
}

type fakeWrite struct {
	p        []byte
	inDialog bool
}

const (
	trustAnchor = "Quick safety check: Is this a project you created or one you trust?"
	trustYes    = "Yes, I trust this folder"
	trustNo     = "No, exit"
)

func (f *startupFake) WriteStdin(p []byte) (int, error) {
	f.mu.Lock()
	f.writes = append(f.writes, fakeWrite{p: append([]byte(nil), p...), inDialog: f.dialog})
	if f.dialog {
		rest := p
		for len(rest) > 0 {
			switch {
			case bytes.HasPrefix(rest, []byte("\x1b[B")):
				if f.marker < len(f.options)-1 {
					f.marker++
				}
				rest = rest[3:]
			case bytes.HasPrefix(rest, []byte("\x1b[A")):
				if f.marker > 0 {
					f.marker--
				}
				rest = rest[3:]
			case bytes.HasPrefix(rest, []byte("\x1b[13u")):
				f.accepted, f.dialog = f.options[f.marker], false
				rest = rest[5:]
			case rest[0] == '\r':
				f.accepted, f.dialog = f.options[f.marker], false
				rest = rest[1:]
			default:
				rest = rest[1:] // typed into the dialog: ignored by it
			}
			if !f.dialog {
				break
			}
		}
	}
	frame := f.frameLocked()
	f.mu.Unlock()
	_, _ = f.d.Write([]byte("\x1b[2J\x1b[H" + frame))
	return len(p), nil
}

func (f *startupFake) frameLocked() string {
	if !f.dialog {
		return "╭────────────────────────────────────────╮\r\n" +
			"│ ❯                                      │\r\n" +
			"╰────────────────────────────────────────╯\r\n" +
			"  ? for shortcuts\r\n"
	}
	var b strings.Builder
	b.WriteString(trustAnchor + "\r\n\r\n")
	for i, o := range f.options {
		if i == f.marker {
			b.WriteString("❯ " + o + "\r\n")
		} else {
			b.WriteString("  " + o + "\r\n")
		}
	}
	b.WriteString("\r\nEnter to confirm · Esc to cancel\r\n")
	return b.String()
}

// isPromptText reports whether a write is typed text rather than a key: no
// escape sequence, and more than a bare Enter.
func isPromptText(p []byte) bool {
	s := strings.Trim(string(p), "\r\n")
	return s != "" && !strings.Contains(s, "\x1b")
}

// TestClaudeScriptsStartFromAnyDirectory runs every canonical claude script
// from each state claude can start in: a trusted directory (straight to the
// composer) and a fresh one (the folder-trust dialog), in both option orders
// claude has shipped — 2.1.251+ puts "No, exit" first and highlighted, 2.1.247
// put "Yes, I trust this folder" first. A script must accept the dialog with
// "Yes, I trust this folder" when it is shown, type nothing into it, and send
// no keys at all into a composer that never showed one.
func TestClaudeScriptsStartFromAnyDirectory(t *testing.T) {
	dir := filepath.Join(repoScriptsRoot(t), "claude")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	starts := []struct {
		name    string
		options []string // nil: trusted directory, no dialog
	}{
		{"trusted directory", nil},
		{"fresh directory, No first (2.1.251+)", []string{trustNo, trustYes}},
		{"fresh directory, Yes first (2.1.247)", []string{trustYes, trustNo}},
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		scr, err := loadScript(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("%s: %v", e.Name(), err)
		}
		// Keep the steps and their order; drop the real pauses.
		for i := range scr.Steps {
			if scr.Steps[i].Sleep != "" {
				scr.Steps[i].Sleep = "1ms"
			}
		}
		for _, st := range starts {
			t.Run(e.Name()+"/"+st.name, func(t *testing.T) {
				d := newScriptDriver(nil, 300*time.Millisecond, 0)
				d.submitKey = submitKeyForHarness("claude-code")
				f := &startupFake{d: d, dialog: st.options != nil, options: st.options}
				d.stdin = f
				// claude paints its first screen on its own, before any input.
				_, _ = d.Write([]byte("\x1b[2J\x1b[H" + f.frameLocked()))

				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				if err := d.Run(ctx, scr); err != nil {
					t.Fatalf("Run: %v", err)
				}

				f.mu.Lock()
				defer f.mu.Unlock()
				if st.options != nil && f.accepted != trustYes {
					t.Errorf("trust dialog answered %q, want %q", f.accepted, trustYes)
				}
				sawPrompt := false
				for _, w := range f.writes {
					if isPromptText(w.p) {
						if w.inDialog {
							t.Errorf("typed %q into the trust dialog", w.p)
						}
						sawPrompt = true
						continue
					}
					if !sawPrompt && !w.inDialog {
						t.Errorf("sent key %q to a composer before the first prompt; a trusted start has nothing to answer", w.p)
					}
				}
				if !sawPrompt {
					t.Error("the script never typed a prompt")
				}
			})
		}
	}
}

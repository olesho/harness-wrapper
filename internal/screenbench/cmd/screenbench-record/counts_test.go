//go:build screenbench

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// scriptKeyCounts is what a script says it will type: prompt bodies, submit
// keys and interrupt keys, one per step (a "text\n" send is a body plus a
// submit, split by the driver).
func scriptKeyCounts(scr *script, submit string) (prompts, submits, interrupts int) {
	for _, st := range scr.Steps {
		switch {
		case st.Interrupt:
			interrupts++
		case st.Send == submit:
			submits++
		case st.Send != "":
			body := strings.TrimSuffix(st.Send, "\n")
			if body != "" && !strings.Contains(body, "\x1b") {
				prompts++
			}
			if strings.HasSuffix(st.Send, "\n") {
				submits++
			}
		}
	}
	return prompts, submits, interrupts
}

// TestClaudeScriptsTypeEachKeyExactlyOnce runs every canonical claude script
// from a trusted start and requires the PTY to receive exactly the prompts,
// submit keys and interrupt keys the script names — no duplicate submit that
// would start a second turn, no dropped interrupt that would record a finished
// reply as an interrupted one.
func TestClaudeScriptsTypeEachKeyExactlyOnce(t *testing.T) {
	const submit = "\x1b[13u"
	dir := filepath.Join(repoScriptsRoot(t), "claude")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		t.Run(e.Name(), func(t *testing.T) {
			scr, err := loadScript(filepath.Join(dir, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			for i := range scr.Steps {
				if scr.Steps[i].Sleep != "" {
					scr.Steps[i].Sleep = "1ms"
				}
			}
			d := newScriptDriver(nil, 200*time.Millisecond, 0)
			d.submitKey = submitKeyForHarness("claude-code")
			d.echoGap = 50 * time.Millisecond
			f := &startupFake{d: d}
			d.stdin = f
			_, _ = d.Write([]byte("\x1b[2J\x1b[H" + f.frameLocked()))
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := d.Run(ctx, scr); err != nil {
				t.Fatalf("Run: %v", err)
			}

			wantP, wantS, wantI := scriptKeyCounts(scr, submit)
			var gotP, gotS, gotI int
			f.mu.Lock()
			for _, w := range f.writes {
				s := string(w.p)
				switch {
				case s == submit:
					gotS++
				case s == "\x03" || s == "\x1b":
					gotI++
				case isPromptText(w.p):
					gotP++
				}
			}
			f.mu.Unlock()
			if gotP != wantP || gotS != wantS || gotI != wantI {
				t.Errorf("typed %d prompts / %d submits / %d interrupts, script names %d / %d / %d", gotP, gotS, gotI, wantP, wantS, wantI)
			}
		})
	}
}

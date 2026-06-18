package claudecode

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/olesho/harness-wrapper/pkg/screen"
)

// Busy is the guard the chat layer's idle-completion fallback consults so it
// never completes a turn while Claude is still working. It keys off the
// "esc to interrupt" footer Claude shows only mid-turn.
func TestBusy_synthetic(t *testing.T) {
	a := New()
	if !a.Busy(screen.Snapshot{Text: "⏵⏵ bypass permissions on · esc to interrupt\n✢ Schlepping… (3s · ↓2 tokens)"}) {
		t.Fatal("expected Busy=true while the 'esc to interrupt' footer is shown")
	}
	if a.Busy(screen.Snapshot{Text: "✻ Baked for 3s\n❯ \n⏵⏵ auto mode on · ← for agents"}) {
		t.Fatal("expected Busy=false at an idle prompt (no 'esc to interrupt')")
	}
}

// Corpus guard: every recording ends at a settled/idle frame, so Busy must be
// false there. If a Claude Code release renames the footer, this fails — the
// early-warning the corpus exists to provide.
func TestBusy_corpusFinalFramesAreIdle(t *testing.T) {
	a := New()
	for _, name := range []string{"tool-call", "multi-turn", "interrupted-mid-reply"} {
		b, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "test", "corpus", "claude-code", name, "bytes.raw"))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		sc := screen.New(120, 40)
		_, _ = sc.Write(b)
		if a.Busy(sc.Snapshot()) {
			t.Errorf("[%s] final frame should be idle (Busy=false), but 'esc to interrupt' was present", name)
		}
	}
}

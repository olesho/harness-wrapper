package claudecode

import (
	"os"
	"path/filepath"
	"testing"
)

// claude 2.1.296 offers, once, at startup, to make auto mode the default when
// the user settings name a defaultMode. testdata/auto-mode-nudge-2.1.296.txt is
// that screen as tmux captured it at 120x40 (settings defaultMode
// bypassPermissions, the working directory's path replaced). It must read as a
// dialog of its own kind: a prompt typed into it confirms the highlighted
// "Yes", which rewrites the user's settings.
func TestDetectInput_AutoModeNudge(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "auto-mode-nudge-2.1.296.txt"))
	if err != nil {
		t.Fatal(err)
	}
	req, det := DetectInputDetail(string(raw))
	if det != DetectOK {
		t.Fatalf("detection = %v, want DetectOK", det)
	}
	if req.Kind != KindAutoModeNudge {
		t.Errorf("Kind = %q, want %q", req.Kind, KindAutoModeNudge)
	}
	if req.Prompt != autoNudgeAnchor {
		t.Errorf("Prompt = %q, want %q", req.Prompt, autoNudgeAnchor)
	}
	want := []struct {
		alias, label, keys string
		highlighted        bool
	}{
		{"proceed", "Yes, set auto mode as my default permission mode", "\r", true},
		{"deny", "No, keep bypass permissions", "\x1b[B\r", false},
	}
	if len(req.Options) != len(want) {
		t.Fatalf("len(Options) = %d, want %d (%+v)", len(req.Options), len(want), req.Options)
	}
	for i, w := range want {
		o := req.Options[i]
		if o.Alias != w.alias || o.Label != w.label || string(o.Keys) != w.keys || o.Highlighted != w.highlighted {
			t.Errorf("Options[%d] = {alias:%q label:%q keys:%q highlighted:%v}, want {alias:%q label:%q keys:%q highlighted:%v}",
				i, o.Alias, o.Label, o.Keys, o.Highlighted, w.alias, w.label, w.keys, w.highlighted)
		}
	}
	if !AnchorPresent(string(raw)) {
		t.Error("AnchorPresent = false on the nudge")
	}
}

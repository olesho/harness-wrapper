package harnesscore

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/pkg/transcript"
)

func textEvent(text string) []transcript.ParsedEvent {
	return []transcript.ParsedEvent{{
		HarnessSessionID: "s",
		Event:            transcript.Event{Type: transcript.EventText, Text: text, Source: transcript.SourceFile},
	}}
}

func drainedTexts(t *testing.T, spool string) []string {
	t.Helper()
	got, err := DrainSpool(spool)
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, pe := range got {
		texts = append(texts, pe.Event.Text)
	}
	return texts
}

// Spool files drain in the order they were written, whatever their hook: with
// the event leading the name, a later "post-tool-use" file sorted ahead of an
// earlier "stop".
func TestDrainSpoolIsChronological(t *testing.T) {
	spool := t.TempDir()
	for _, w := range []struct{ event, text string }{
		{"stop", "first"},
		{HookArgPostToolUse, "second"},
		{"session-start", "third"},
		{HookArgPreToolUse, "fourth"},
	} {
		if err := writeSpool(spool, w.event, textEvent(w.text)); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Millisecond) // distinct timestamps
	}
	if got, want := drainedTexts(t, spool), []string{"first", "second", "third", "fourth"}; !slices.Equal(got, want) {
		t.Fatalf("drain order = %v, want %v", got, want)
	}
}

// ReadSpool returns its batches in the order they were written too.
func TestReadSpoolIsChronological(t *testing.T) {
	spool := t.TempDir()
	for _, w := range []struct{ event, text string }{{"stop", "first"}, {HookArgPostToolUse, "second"}} {
		if err := writeSpool(spool, w.event, textEvent(w.text)); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Millisecond)
	}
	sc, err := ReadSpool(spool)
	if err != nil {
		t.Fatal(err)
	}
	if got := batchTexts(sc.Batches); !slices.Equal(got, []string{"first", "second"}) {
		t.Fatalf("read order = %v", got)
	}
}

// A spool written before the change may still hold legacy event-first names.
// They are read by their own timestamp, so they come before current files
// written after them, though "s…" sorts after every digit.
func TestDrainSpoolOrdersLegacyNames(t *testing.T) {
	spool := t.TempDir()
	legacy := filepath.Join(spool, "stop-1000-7-1.json")
	data, err := transcript.MarshalParsedEvents(textEvent("legacy"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeSpool(spool, "stop", textEvent("current")); err != nil {
		t.Fatal(err)
	}
	if got := drainedTexts(t, spool); !slices.Equal(got, []string{"legacy", "current"}) {
		t.Fatalf("drain order = %v, want [legacy current]", got)
	}
}

// WriteSpoolFile keeps the hook and the time it is given, so events moved to
// another spool keep their dispatch and their order; it refuses a name that
// would not parse back.
func TestWriteSpoolFileKeepsEventAndTime(t *testing.T) {
	spool := t.TempDir()
	if err := WriteSpoolFile(spool, HookArgPostToolUse, 42, textEvent("x")); err != nil {
		t.Fatal(err)
	}
	for name := range spoolJSON(t, spool) {
		if event, nanos, ok := ParseSpoolFileName(name); !ok || event != HookArgPostToolUse || nanos != 42 {
			t.Errorf("%s parses to %q, %d, %v", name, event, nanos, ok)
		}
	}
	for _, bad := range []string{"", "../x", "9lives"} {
		if err := WriteSpoolFile(spool, bad, 1, textEvent("x")); err == nil {
			t.Errorf("WriteSpoolFile(%q) wrote a file", bad)
		}
	}
	if err := WriteSpoolFile(spool, "stop", -1, textEvent("x")); err == nil {
		t.Error("WriteSpoolFile took a time before 1970")
	}
}

func TestParseSpoolFileName(t *testing.T) {
	for _, c := range []struct {
		name  string
		event string
		nanos int64
		ok    bool
	}{
		{"00000000000000001234-post-tool-use-failure-42-7.json", HookArgPostToolUseFailure, 1234, true},
		{"00000000000000001234-post-tool-use-42-7.json", HookArgPostToolUse, 1234, true},
		{"00000000000000000009-stop-1-1.json", "stop", 9, true},
		{"post-tool-use-failure-1234-42-7.json", HookArgPostToolUseFailure, 1234, true},
		{"pre-tool-use-1-1-1.json", HookArgPreToolUse, 1, true},
		{"stop-5-1-1.json", "stop", 5, true},
		{"stop-5-1-1.json.tmp", "", 0, false},
		{"stop.json", "", 0, false},
		{"00000000000000001234-42-7.json", "", 0, false},
		{"stop-x-1-1.json", "", 0, false},
		{"session-marker.json", "", 0, false},
	} {
		event, nanos, ok := ParseSpoolFileName(c.name)
		if event != c.event || nanos != c.nanos || ok != c.ok {
			t.Errorf("ParseSpoolFileName(%q) = %q, %d, %v; want %q, %d, %v", c.name, event, nanos, ok, c.event, c.nanos, c.ok)
		}
	}
	// What writeSpool names parses back to its event.
	spool := t.TempDir()
	if err := writeSpool(spool, HookArgSubagentStop, textEvent("x")); err != nil {
		t.Fatal(err)
	}
	for name := range spoolJSON(t, spool) {
		if event, _, ok := ParseSpoolFileName(name); !ok || event != HookArgSubagentStop {
			t.Errorf("%s parses to %q, %v", name, event, ok)
		}
	}
}

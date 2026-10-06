package chatcore

import (
	"reflect"
	"testing"
)

// TestReopenOptionsMirrorsOptions fails when an Options field has no
// ReopenOptions counterpart. A field added to Options but not here silently
// drops that behavior on reopen: it once lost AutoSkipCodexUpdateNotice, so a
// reopened codex session surfaced the update menu the original suppressed.
//
// The exempt fields are the ones Reopen fills from the stored record (Harness,
// WorkingDir, Resume) or that only make sense for a fresh session
// (HarnessSessionID names the session Open starts; Reopen resumes one).
func TestReopenOptionsMirrorsOptions(t *testing.T) {
	fromRecord := map[string]bool{
		"Harness":          true,
		"WorkingDir":       true,
		"Resume":           true,
		"HarnessSessionID": true,
	}
	ro := reflect.TypeOf(ReopenOptions{})
	ot := reflect.TypeOf(Options{})
	for i := 0; i < ot.NumField(); i++ {
		f := ot.Field(i)
		if fromRecord[f.Name] {
			if _, ok := ro.FieldByName(f.Name); ok {
				t.Errorf("ReopenOptions.%s exists, but Reopen takes %s from the stored record", f.Name, f.Name)
			}
			continue
		}
		rf, ok := ro.FieldByName(f.Name)
		if !ok {
			t.Errorf("Options.%s has no ReopenOptions counterpart", f.Name)
			continue
		}
		if rf.Type != f.Type {
			t.Errorf("ReopenOptions.%s is %s, Options.%s is %s", f.Name, rf.Type, f.Name, f.Type)
		}
	}
}

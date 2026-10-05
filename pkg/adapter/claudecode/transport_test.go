package claudecode

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/olesho/harness-wrapper/pkg/contract"
)

// claude's list of background tasks, in the contract's terms: a shell is a
// command, an agent a subagent, anything else other; a task with no id is
// dropped, a long description cut on a rune, and no list is an empty one.
func TestBackgroundTasks(t *testing.T) {
	long := strings.Repeat("é", maxTaskDescription)
	got := backgroundTasks(json.RawMessage(`[
		{"task_id": "b1", "task_type": "local_bash", "description": "build"},
		{"task_id": "a1", "task_type": "local_agent", "description": "` + long + `"},
		{"task_id": "r1", "task_type": "remote_agent"},
		{"task_id": "m1", "task_type": "monitor"},
		{"task_type": "local_bash"}
	]`))
	want := []contract.BackgroundKind{contract.BackgroundCommand, contract.BackgroundSubagent, contract.BackgroundSubagent, contract.BackgroundOther}
	if len(got) != len(want) {
		t.Fatalf("tasks %+v", got)
	}
	for i, k := range want {
		if got[i].Kind != k {
			t.Errorf("task %s is %s, want %s", got[i].ID, got[i].Kind, k)
		}
	}
	if d := got[1].Description; len(d) > maxTaskDescription || !utf8.ValidString(d) || d == "" {
		t.Errorf("a long description cut to %d bytes, valid %v", len(d), utf8.ValidString(d))
	}
	if none := backgroundTasks(nil); none == nil || len(none) != 0 {
		t.Errorf("no list: %#v, want an empty one", none)
	}
}

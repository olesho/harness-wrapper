package claudecode

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/internal/sessionid"
	"github.com/olesho/harness-wrapper/pkg/adapter"
	"github.com/olesho/harness-wrapper/pkg/contract"
)

// scriptedClaude speaks just enough of claude's stream-json to open and
// take a turn, as the session $EARLY_ID names before it answers initialize
// (if set) and $INIT_ID names at the turn's system/init.
const scriptedClaude = `#!/bin/sh
while IFS= read -r line; do
  case "$line" in
  *'"subtype":"initialize"'*)
    [ -n "$EARLY_ID" ] && printf '{"type":"system","subtype":"hook_started","session_id":"%s"}\n' "$EARLY_ID"
    printf '{"type":"control_response","response":{"subtype":"success","request_id":"req_1","response":{}}}\n'
    ;;
  *'"type":"user"'*)
    uuid=$(printf '%s' "$line" | sed 's/.*"uuid":"\([^"]*\)".*/\1/')
    printf '{"type":"command_lifecycle","command_uuid":"%s","state":"queued"}\n' "$uuid"
    printf '{"type":"system","subtype":"init","session_id":"%s","capabilities":["interrupt_receipt_v1","msg_lifecycle_v1"]}\n' "$INIT_ID"
    printf '{"type":"command_lifecycle","command_uuid":"%s","state":"started"}\n' "$uuid"
    printf '{"type":"result","subtype":"success","is_error":false,"result":"PONG","session_id":"%s"}\n' "$INIT_ID"
    ;;
  esac
done
`

// startScripted starts the scripted claude for Session id, the session ids it
// names set in its environment.
func startScripted(t *testing.T, id string, env []string, report func(adapter.Event)) (adapter.Transport, error) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude")
	if err := os.WriteFile(bin, []byte(scriptedClaude), 0o755); err != nil {
		t.Fatal(err)
	}
	oc, _ := json.Marshal(openConfig{Binary: bin, Env: env, WorkingDir: dir, Spool: filepath.Join(dir, "scratch")})
	return Profile{}.Start(context.Background(), adapter.Start{Mode: contract.OpenFresh, SessionID: id, OpenConfig: oc, Report: report})
}

// A claude that speaks for a session other than the Session's — a copy, as
// claude may start of a session another process holds — is stopped: before
// it answered its initialize request the open fails with session_in_use, and
// at a turn's system/init the turn ends errored; claude speaking for the
// Session's own id, in any case, takes the turn.
func TestForeignSessionStops(t *testing.T) {
	id, other := sessionid.NewUUID(), sessionid.NewUUID()

	_, err := startScripted(t, id, []string{"EARLY_ID=" + other, "INIT_ID=" + id}, func(adapter.Event) {})
	var e *contract.Error
	if !errors.As(err, &e) || e.Code != contract.CodeOpenFailed || e.Reason != contract.OpenSessionInUse {
		t.Fatalf("a claude that opened another session: %v, want open_failed session_in_use", err)
	}

	for name, init := range map[string]string{"own": strings.ToUpper(id), "foreign": other} {
		events := make(chan adapter.Event, 16)
		tr, err := startScripted(t, id, []string{"EARLY_ID=" + id, "INIT_ID=" + init}, func(ev adapter.Event) { events <- ev })
		if err != nil {
			t.Fatalf("%s: Start: %v", name, err)
		}
		native := sessionid.NewUUID()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := tr.Submit(ctx, adapter.Submission{Native: native, Text: "PING 1"}); err != nil {
			t.Fatalf("%s: Submit: %v", name, err)
		}
		var ended *adapter.Event
		exited := false
		for ended == nil || name == "foreign" && !exited {
			select {
			case ev := <-events:
				switch ev.Kind {
				case adapter.Ended:
					ended = &ev
				case adapter.Exited:
					exited = true
					if !strings.Contains(ev.Exit.Detail, "runs session "+other) {
						t.Errorf("%s: exit detail %q names no other session", name, ev.Exit.Detail)
					}
				}
			case <-ctx.Done():
				t.Fatalf("%s: ended %v, exited %v", name, ended, exited)
			}
		}
		cancel()
		switch {
		case ended.Native != native:
			t.Errorf("%s: the end of %s, want %s", name, ended.Native, native)
		case name == "own" && (ended.Outcome != contract.TurnCompleted || ended.Text != "PONG"):
			t.Errorf("own: %s %q, want completed with PONG", ended.Outcome, ended.Text)
		case name == "foreign" && ended.Outcome != contract.TurnErrored:
			t.Errorf("foreign: %s, want errored", ended.Outcome)
		}
		tr.Stop(context.Background(), time.Second)
	}
}

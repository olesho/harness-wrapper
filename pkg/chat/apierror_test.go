package chat

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/olesho/harness-wrapper/pkg/screen"
	"github.com/olesho/harness-wrapper/pkg/transcript"
	transcriptcc "github.com/olesho/harness-wrapper/pkg/transcript/claudecode"
	"github.com/olesho/harness-wrapper/pkg/turns/harness/claudecode"
	"github.com/olesho/harness-wrapper/pkg/turns/harness/opencode"
)

// ─── fixtures ────────────────────────────────────────────────────────────────

// userLine / replyLine / errorLine build the three Claude Code JSONL shapes
// this file needs. The error line's field names and layout are copied from a
// real capture (test/corpus/apierror); only the text and the tag vary.
func userLine(text string) string {
	return `{"type":"user","uuid":"u","message":{"role":"user","content":` + quote(text) + `},"timestamp":"2026-09-01T00:00:00Z"}`
}

func replyLine(text string) string {
	return `{"type":"assistant","uuid":"a","message":{"role":"assistant","model":"claude-opus-5","content":[{"type":"text","text":` + quote(text) + `}]},"timestamp":"2026-09-01T00:00:01Z"}`
}

func errorLine(tag, text string) string {
	return `{"type":"assistant","uuid":"e","message":{"role":"assistant","model":"<synthetic>","content":[{"type":"text","text":` + quote(text) + `}]},"timestamp":"2026-09-01T00:00:02Z","isApiErrorMessage":true,"error":` + quote(tag) + `}`
}

func quote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s) + `"`
}

// convWithTranscript builds a Conversation whose claude-code adapter reads a
// transcript written to a temp dir, so the tag path can be exercised end to end
// without a harness.
func convWithTranscript(t *testing.T, sessionID string, lines ...string) *Conversation {
	t.Helper()
	root := t.TempDir()
	wd := t.TempDir()
	projDir := filepath.Join(root, transcriptcc.EncodedCWD(mustEvalSymlinks(t, wd)))
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := ""
	for _, l := range lines {
		body += l + "\n"
	}
	if err := os.WriteFile(filepath.Join(projDir, sessionID+".jsonl"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	a := claudecode.New()
	a.ProjectsRoot = root
	c := &Conversation{
		opts:                    Options{Harness: chatClaudeCode, WorkingDir: wd},
		adapter:                 a,
		sentTranscriptWatermark: 0,
		closed:                  make(chan struct{}),
	}
	c.session.setHarnessID(sessionID)
	return c
}

func mustEvalSymlinks(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		return p
	}
	return r
}

// ─── the mapping ─────────────────────────────────────────────────────────────

// TestAPIErrorVerdict_WallsAndFailures covers the whole vocabulary in one
// table: which tags are walls (coded), which merely error the turn, and which
// yield nothing at all.
func TestAPIErrorVerdict_WallsAndFailures(t *testing.T) {
	cases := []struct {
		tag        string
		wantOK     bool
		wantCode   TurnCode
		wantReason string
	}{
		{"billing_error", true, CodeBillingWall, ReasonBillingWall},
		{"account_on_hold", true, CodeBillingWall, ReasonBillingWall},
		{"authentication_failed", true, CodeAuthRequired, ReasonAuthRequired},
		{"oauth_org_not_allowed", true, CodeAuthRequired, ReasonAuthRequired},
		{"verification_required", true, CodeAuthRequired, ReasonAuthRequired},
		{"cloud_credential_error", true, CodeAuthRequired, ReasonAuthRequired},
		{"rate_limit", true, CodeUsageLimited, ReasonUsageLimited},
		{"server_error", true, "", ""},
		{"overloaded", true, "", ""},
		{"model_not_found", true, "", ""},
		// Deliberately unmapped: no verdict, so the screen relabels keep the turn.
		{"invalid_request", false, "", ""},
		{"max_output_tokens", false, "", ""},
		{"unknown", false, "", ""},
		{"policy_denied", false, "", ""},
		{"a_tag_from_a_future_claude", false, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.tag, func(t *testing.T) {
			turns := []transcript.Turn{{Role: string(RoleAssistant), Text: "rendered error", APIError: tc.tag}}
			v, ok, _ := apiErrorVerdictFrom(turns, 0)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if v.code != tc.wantCode {
				t.Errorf("code = %q, want %q", v.code, tc.wantCode)
			}
			if v.reason != tc.wantReason {
				t.Errorf("reason = %q, want %q", v.reason, tc.wantReason)
			}
			// The harness's own words are the evidence for the verdict.
			r := v.turnReason(chatClaudeCode)
			if !strings.Contains(r, tc.tag) || !strings.Contains(r, "rendered error") {
				t.Errorf("turn reason = %q, want it to carry the tag and the rendered text", r)
			}
		})
	}
}

// TestAPIErrorVerdict_LastWordOnly is the rule that keeps a recovered turn a
// success: claude retries a 529 internally, so a transcript holding BOTH the
// error and the reply that followed it is a completed turn.
func TestAPIErrorVerdict_LastWordOnly(t *testing.T) {
	t.Run("error then reply is not a failure", func(t *testing.T) {
		turns := []transcript.Turn{
			{Role: string(RoleAssistant), Text: "API Error: 529 Overloaded", APIError: "server_error"},
			{Role: string(RoleAssistant), Text: "Done — the migration is applied."},
		}
		if v, ok, _ := apiErrorVerdictFrom(turns, 0); ok {
			t.Fatalf("got verdict %+v, want none: the turn recovered", v)
		}
	})
	t.Run("reply then error is a failure", func(t *testing.T) {
		turns := []transcript.Turn{
			{Role: string(RoleAssistant), Text: "Working on it…"},
			{Role: string(RoleAssistant), Text: "API Error: Connection lost mid-response.", APIError: "server_error"},
		}
		if _, ok, _ := apiErrorVerdictFrom(turns, 0); !ok {
			t.Fatal("no verdict: a turn that ended on an error did not complete")
		}
	})
	t.Run("non-assistant entries after the error are skipped", func(t *testing.T) {
		turns := []transcript.Turn{
			{Role: string(RoleAssistant), Text: "Login expired · Please run /login", APIError: "authentication_failed"},
			{Role: string(RoleSystem), Text: "some system note"},
		}
		v, ok, _ := apiErrorVerdictFrom(turns, 0)
		if !ok || v.code != CodeAuthRequired {
			t.Fatalf("verdict = %+v/%v, want auth_required", v, ok)
		}
	})
}

// TestAPIErrorVerdict_Correlation is the other half: a tag from a PREVIOUS turn
// must never condemn this one. Resumed sessions carry every earlier turn's
// entries, which is exactly how a stale banner used to stop a healthy agent one
// layer down.
func TestAPIErrorVerdict_Correlation(t *testing.T) {
	// A resumed session: the login expired last time, then it was renewed and
	// this turn answered.
	resumed := []transcript.Turn{
		{Role: string(RoleUser), Text: "previous prompt"},
		{Role: string(RoleAssistant), Text: "Login expired · Please run /login", APIError: "authentication_failed"},
		{Role: string(RoleUser), Text: "this turn's prompt"},
		{Role: string(RoleAssistant), Text: "Done."},
	}
	if v, ok, _ := apiErrorVerdictFrom(resumed, 2); ok {
		t.Errorf("got verdict %+v at watermark 2, want none — the tag belongs to an earlier turn", v)
	}
	// Same transcript, watermark 0: without correlation the stale tag decides,
	// which is the failure the watermark exists to prevent. (Here the later
	// reply saves it, so assert the sharper shape below.)
	stale := []transcript.Turn{
		{Role: string(RoleAssistant), Text: "Login expired · Please run /login", APIError: "authentication_failed"},
	}
	if _, ok, _ := apiErrorVerdictFrom(stale, 1); ok {
		t.Error("an entry BEFORE the watermark decided the turn")
	}
	if _, ok, _ := apiErrorVerdictFrom(stale, 0); !ok {
		t.Error("an entry AT the watermark should decide the turn")
	}
	if _, ok, _ := apiErrorVerdictFrom(stale, watermarkUnknown); ok {
		t.Error("an unknown watermark must yield no verdict, not a verdict over everything")
	}
}

// ─── the conversation paths ──────────────────────────────────────────────────

// TestAPIErrorRelabel_ErrorsAFalseSuccess is the headline behaviour change: a
// turn the harness recorded as failed no longer completes with the error
// message as its reply.
func TestAPIErrorRelabel_ErrorsAFalseSuccess(t *testing.T) {
	c := convWithTranscript(
		t, "sess-1",
		userLine("do the thing"),
		errorLine("authentication_failed", "Login expired · Please run /login"),
	)
	turn := &Turn{
		State:  TurnStateComplete,
		Reason: "claude-code: end-of-turn marker confirmed at a settled prompt",
		Text:   "Login expired · Please run /login",
	}
	if !c.apiErrorRelabel(turn) {
		t.Fatal("apiErrorRelabel declined a tagged failure")
	}
	if turn.State != TurnStateErrored {
		t.Errorf("State = %q, want errored", turn.State)
	}
	if turn.Code != CodeAuthRequired {
		t.Errorf("Code = %q, want %q", turn.Code, CodeAuthRequired)
	}
	if !strings.HasPrefix(turn.Reason, ReasonAuthRequired) {
		t.Errorf("Reason = %q, want the ReasonAuthRequired prefix", turn.Reason)
	}
	if turn.Text != "" {
		t.Errorf("Text = %q, want empty — an API error is not a reply", turn.Text)
	}
}

// TestAPIErrorRelabel_BillingReachesTheWall is the one class that had no
// representation at all before: no reason, no code, and a marker downstream
// with nothing to emit it.
func TestAPIErrorRelabel_BillingReachesTheWall(t *testing.T) {
	c := convWithTranscript(
		t, "sess-b",
		userLine("do the thing"),
		errorLine("billing_error", "Credit balance too low · Add funds: https://platform.claude.com/settings/billing"),
	)
	turn := &Turn{State: TurnStateComplete, Text: "Credit balance too low"}
	if !c.apiErrorRelabel(turn) {
		t.Fatal("apiErrorRelabel declined a billing_error")
	}
	if turn.Code != CodeBillingWall {
		t.Fatalf("Code = %q, want %q", turn.Code, CodeBillingWall)
	}
	if !strings.HasPrefix(turn.Reason, ReasonBillingWall) {
		t.Errorf("Reason = %q, want the ReasonBillingWall prefix", turn.Reason)
	}
	// No screen recogniser produced this: the transcript is the only source.
	if authRequired(chatClaudeCode, "Credit balance too low · Add funds: https://platform.claude.com/settings/billing") {
		t.Error("precondition: the billing banner must NOT be an auth banner")
	}
}

// TestAPIErrorRelabel_DeclinesWhenItCannotCorrelate asserts every path with no
// usable evidence leaves the turn exactly as it was — which is today's
// behaviour, and the direction this must fail in.
func TestAPIErrorRelabel_DeclinesWhenItCannotCorrelate(t *testing.T) {
	complete := func() *Turn {
		return &Turn{State: TurnStateComplete, Text: "a real reply", Reason: "done"}
	}

	t.Run("no transcript reader", func(t *testing.T) {
		// opencode implements no TranscriptReader — there is no recorded
		// rollout format for it yet — so this is the real shape of "the
		// adapter cannot read the harness's own log", not a stub of it.
		c := &Conversation{opts: Options{Harness: "opencode"}, adapter: opencode.New(), closed: make(chan struct{})}
		turn := complete()
		if c.apiErrorRelabel(turn) || turn.State != TurnStateComplete {
			t.Error("relabelled a turn with no transcript reader")
		}
	})
	t.Run("no harness session id", func(t *testing.T) {
		c := convWithTranscript(t, "sess-2", errorLine("authentication_failed", "Login expired"))
		c.session.setHarnessID("")
		turn := complete()
		if c.apiErrorRelabel(turn) || turn.State != TurnStateComplete {
			t.Error("relabelled a turn with no harness session id")
		}
	})
	t.Run("no watermark", func(t *testing.T) {
		c := convWithTranscript(t, "sess-3", errorLine("authentication_failed", "Login expired"))
		c.sentTranscriptWatermark = watermarkUnknown
		turn := complete()
		if c.apiErrorRelabel(turn) || turn.State != TurnStateComplete {
			t.Error("relabelled a turn with no pre-send watermark")
		}
	})
	t.Run("transcript unreadable", func(t *testing.T) {
		c := convWithTranscript(t, "sess-4", errorLine("authentication_failed", "Login expired"))
		c.session.setHarnessID("no-such-session")
		turn := complete()
		if c.apiErrorRelabel(turn) || turn.State != TurnStateComplete {
			t.Error("relabelled a turn whose transcript could not be read")
		}
	})
	t.Run("a genuine reply", func(t *testing.T) {
		c := convWithTranscript(t, "sess-5", userLine("go"), replyLine("Done — applied the patch."))
		turn := complete()
		if c.apiErrorRelabel(turn) || turn.State != TurnStateComplete {
			t.Error("relabelled a turn that really did complete")
		}
	})
}

// testStore is the minimum Store the terminal paths touch. pkg/chat/memstore
// imports pkg/chat, so an in-package test cannot use it.
type testStore struct{ turns map[string]*Turn }

func newTestStore() *testStore { return &testStore{turns: map[string]*Turn{}} }

func (s *testStore) CreateSession(context.Context, *Session) error { return nil }
func (s *testStore) GetSession(context.Context, string) (*Session, error) {
	return &Session{}, nil
}
func (s *testStore) UpdateSession(context.Context, *Session) error { return nil }
func (s *testStore) AppendTurn(_ context.Context, t *Turn) error {
	cp := *t
	s.turns[t.ID] = &cp
	return nil
}

func (s *testStore) UpdateTurn(_ context.Context, t *Turn) error {
	cp := *t
	s.turns[t.ID] = &cp
	return nil
}

func (s *testStore) ListTurns(context.Context, string) ([]Turn, error) { return nil, nil }

// TestRelabelTerminal_Order pins the strength order. The harness's own verdict
// outranks both screen readings, so a screen showing a usage wall while the
// transcript records a billing failure resolves to BILLING: renewing quota
// would not help, and the wall the operator must act on is the one the harness
// named.
func TestRelabelTerminal_Order(t *testing.T) {
	const wall = "You've hit your session limit · resets 10:20pm (Europe/Warsaw)"
	c := convWithTranscript(
		t, "sess-order",
		userLine("go"),
		errorLine("billing_error", "Credit balance too low"),
	)
	snap := screen.Snapshot{Text: "⏺ " + wall + "\n\n✻ Brewed for 0s\n\n❯ \n"}

	// Precondition: the screen alone WOULD say usage-limited.
	probe := &Turn{State: TurnStateComplete, Text: wall}
	if !c.usageLimitRelabel(probe, snap) {
		t.Fatal("precondition: the screen should read as a usage wall")
	}

	turn := &Turn{State: TurnStateComplete, Text: wall}
	if !c.relabelTerminal(turn, snap) {
		t.Fatal("relabelTerminal declined")
	}
	if turn.Code != CodeBillingWall {
		t.Errorf("Code = %q, want %q — the harness's verdict outranks the screen", turn.Code, CodeBillingWall)
	}
}

// TestRelabelTerminal_FallsBackToTheScreen asserts the screen relabels still
// run, unchanged, when the transcript says nothing. Every path without a
// transcript keeps exactly the behaviour it has today.
func TestRelabelTerminal_FallsBackToTheScreen(t *testing.T) {
	c := convWithTranscript(t, "sess-fb", userLine("go"), replyLine("Working…"))

	t.Run("usage wall", func(t *testing.T) {
		const wall = "You've hit your session limit · resets 8pm (UTC)"
		snap := screen.Snapshot{Text: "⏺ " + wall + "\n\n❯ \n"}
		turn := &Turn{State: TurnStateComplete, Text: wall}
		if !c.relabelTerminal(turn, snap) {
			t.Fatal("relabelTerminal declined a usage wall")
		}
		if turn.Code != CodeUsageLimited || !strings.HasPrefix(turn.Reason, ReasonUsageLimited) {
			t.Errorf("turn = {Code:%q Reason:%q}, want the usage wall", turn.Code, turn.Reason)
		}
	})
	t.Run("logged-out screen", func(t *testing.T) {
		snap := screen.Snapshot{Text: "Claude Code\n\nNot logged in · Run /login\n\n❯ \n"}
		turn := &Turn{State: TurnStateComplete}
		if !c.relabelTerminal(turn, snap) {
			t.Fatal("relabelTerminal declined a logged-out screen")
		}
		if turn.Code != CodeAuthRequired || turn.Reason != ReasonAuthRequired {
			t.Errorf("turn = {Code:%q Reason:%q}, want the auth wall", turn.Code, turn.Reason)
		}
	})
	t.Run("a genuine reply is untouched", func(t *testing.T) {
		snap := screen.Snapshot{Text: "⏺ Done — the tests pass.\n\n✻ Brewed for 2s\n\n❯ \n"}
		turn := &Turn{State: TurnStateComplete, Text: "Done — the tests pass."}
		if c.relabelTerminal(turn, snap) {
			t.Fatal("relabelTerminal fired on a genuine reply")
		}
		if turn.State != TurnStateComplete || turn.Code != "" {
			t.Errorf("turn = {State:%q Code:%q}, want an untouched completion", turn.State, turn.Code)
		}
	})
}

// ─── the swallowed-prompt proof ──────────────────────────────────────────────

// TestTranscriptProof_RejectsATaggedEntry covers the bug the tag also fixes:
// tryTranscriptProof accepted ANY assistant entry with non-empty text as proof
// that a seemingly swallowed prompt in fact completed — and a synthetic API
// error has text, the RENDERED error. It rescued failures into successes.
func TestTranscriptProof_RejectsATaggedEntry(t *testing.T) {
	c := convWithTranscript(
		t, "sess-proof",
		userLine("go"),
		errorLine("server_error", "API Error: 529 Overloaded"),
	)
	v := c.transcriptProofOfCurrentTurn(screen.Snapshot{Text: "❯ \n"})
	if v.proofText != "" {
		t.Fatalf("proofText = %q, want empty — an API error is not proof of completion", v.proofText)
	}
	if !strings.Contains(v.diag, "no assistant output") {
		t.Errorf("diag = %q, want it to report no assistant output", v.diag)
	}

	// ...and a real reply still proves it.
	ok := convWithTranscript(t, "sess-proof-ok", userLine("go"), replyLine("Done."))
	if got := ok.transcriptProofOfCurrentTurn(screen.Snapshot{Text: "❯ \n"}); got.proofText != "Done." {
		t.Errorf("proofText = %q, want %q", got.proofText, "Done.")
	}
}

// TestSwallowedVerdict_TagNamesTheFailure asserts the swallowed path reports
// the harness's own verdict rather than the generic "prompt not accepted".
func TestSwallowedVerdict_TagNamesTheFailure(t *testing.T) {
	c := convWithTranscript(
		t, "sess-sw",
		userLine("go"),
		errorLine("billing_error", "Credit balance too low"),
	)
	c.store = newTestStore()
	c.eventCh = make(chan ConversationEvent, 4)
	turn := &Turn{ID: "t1", State: TurnStatePending}
	c.applySwallowedPromptVerdict(turn, screen.Snapshot{Text: "❯ \n"})
	if turn.State != TurnStateErrored {
		t.Fatalf("State = %q, want errored", turn.State)
	}
	if turn.Code != CodeBillingWall {
		t.Errorf("Code = %q, want %q", turn.Code, CodeBillingWall)
	}
}

// ─── the codes, at every producer ────────────────────────────────────────────

// TestEveryReasonCarriesItsCode is the guard behind "choose the marker from the
// code, never the prose": a producer that sets a wall Reason without its Code
// makes a consumer silently fall back to pattern inference. Reason is operator
// copy and gets reworded; Code is the contract.
func TestEveryReasonCarriesItsCode(t *testing.T) {
	c := &Conversation{opts: Options{Harness: chatClaudeCode}, adapter: claudecode.New()}

	t.Run("authRelabel", func(t *testing.T) {
		snap := screen.Snapshot{Text: "Claude Code\n\nNot logged in · Run /login\n\n❯ \n"}
		turn := &Turn{State: TurnStateComplete}
		if !c.authRelabel(turn, snap) {
			t.Fatal("authRelabel declined")
		}
		assertReasonCode(t, turn, ReasonAuthRequired, CodeAuthRequired)
	})
	t.Run("usageLimitRelabel", func(t *testing.T) {
		snap := screen.Snapshot{Text: "⏺ You've hit your session limit · resets 8pm (UTC)\n\n❯ \n"}
		turn := &Turn{State: TurnStateComplete, Text: "You've hit your session limit · resets 8pm (UTC)"}
		if !c.usageLimitRelabel(turn, snap) {
			t.Fatal("usageLimitRelabel declined")
		}
		assertReasonCode(t, turn, ReasonUsageLimited, CodeUsageLimited)
	})
	t.Run("apiErrorRelabel", func(t *testing.T) {
		tc := convWithTranscript(t, "sess-codes", userLine("go"), errorLine("rate_limit", "API Error: 429"))
		turn := &Turn{State: TurnStateComplete}
		if !tc.apiErrorRelabel(turn) {
			t.Fatal("apiErrorRelabel declined")
		}
		assertReasonCode(t, turn, ReasonUsageLimited, CodeUsageLimited)
	})
	t.Run("swallowed verdict on a logged-out screen", func(t *testing.T) {
		sc := convWithTranscript(t, "sess-codes-sw", userLine("go"))
		sc.store = newTestStore()
		sc.eventCh = make(chan ConversationEvent, 4)
		turn := &Turn{ID: "t", State: TurnStatePending}
		sc.applySwallowedPromptVerdict(turn, screen.Snapshot{Text: "Not logged in · Run /login\n❯ \n"})
		assertReasonCode(t, turn, ReasonAuthRequired, CodeAuthRequired)
	})
}

func assertReasonCode(t *testing.T, turn *Turn, wantReason string, wantCode TurnCode) {
	t.Helper()
	if !strings.HasPrefix(turn.Reason, wantReason) {
		t.Errorf("Reason = %q, want the %q prefix", turn.Reason, wantReason)
	}
	if turn.Code != wantCode {
		t.Errorf("Code = %q, want %q", turn.Code, wantCode)
	}
}

// ─── parser compatibility ────────────────────────────────────────────────────

// TestParserCompatibility_TagAppearsOnlyWhereTheLineCarriedIt asserts the two
// new fields are additive in the strict sense: an ordinary transcript parses to
// exactly what it parsed to before.
func TestParserCompatibility_TagAppearsOnlyWhereTheLineCarriedIt(t *testing.T) {
	body := userLine("hello") + "\n" + replyLine("hi there") + "\n" +
		errorLine("server_error", "API Error: 500") + "\n" + replyLine("recovered") + "\n"
	evs, err := transcriptcc.Events([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 4 {
		t.Fatalf("got %d events, want 4", len(evs))
	}
	for i, want := range []string{"", "", "server_error", ""} {
		if evs[i].APIError != want {
			t.Errorf("event %d APIError = %q, want %q", i, evs[i].APIError, want)
		}
	}
}

// TestParserCompatibility_ErrorWithoutTheFlagIsNotAnAPIError is the guard that
// keeps hook results out of the mapping. `error` also carries "warn", "debug"
// and "policy_denied" on lines that are not API errors, and reading one of
// those as a failed turn would error a turn the harness completed.
func TestParserCompatibility_ErrorWithoutTheFlagIsNotAnAPIError(t *testing.T) {
	line := `{"type":"assistant","uuid":"x","message":{"role":"assistant","content":[{"type":"text","text":"all good"}]},"timestamp":"2026-09-01T00:00:00Z","error":"warn"}`
	evs, err := transcriptcc.Events([]byte(line + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 {
		t.Fatalf("got %d events, want 1", len(evs))
	}
	if evs[0].APIError != "" {
		t.Errorf("APIError = %q, want empty: isApiErrorMessage was not set", evs[0].APIError)
	}
}

// TestParserCompatibility_EmptyTagYieldsNoVerdict covers the other conservative
// edge: a line flagged as an API error but carrying no tag says nothing.
func TestParserCompatibility_EmptyTagYieldsNoVerdict(t *testing.T) {
	line := `{"type":"assistant","uuid":"x","message":{"role":"assistant","content":[{"type":"text","text":"API Error: something"}]},"timestamp":"2026-09-01T00:00:00Z","isApiErrorMessage":true}`
	evs, err := transcriptcc.Events([]byte(line + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	if evs[0].APIError != "" {
		t.Fatalf("APIError = %q, want empty", evs[0].APIError)
	}
	if _, ok, _ := apiErrorVerdictFrom(transcript.TurnsFromEvents(evs), 0); ok {
		t.Error("an untagged API-error line produced a verdict")
	}
}

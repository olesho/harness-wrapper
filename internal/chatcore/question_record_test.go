package chatcore

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/pkg/harnessenv"
	"github.com/olesho/harness-wrapper/pkg/harnessname"
	transcriptcc "github.com/olesho/harness-wrapper/pkg/transcript/claudecode"
	"github.com/olesho/harness-wrapper/pkg/turns/harness/claudecode"
)

// TestQuestionLive answers claude's AskUserQuestion dialog through Answer,
// against the real claude binary pointed at a local Messages API that asks
// the question — no account, no tokens:
//
//	HW_LIVE_QUESTION=1 go test ./internal/chatcore -run QuestionLive -v
//
// TestRecordQuestion runs the same scenarios and also records them into
// test/corpus/claude-code (bytes.raw, meta.json, transcript.jsonl):
//
//	HW_RECORD_QUESTION=1 go test ./internal/chatcore -run RecordQuestion -v
func TestQuestionLive(t *testing.T) {
	if os.Getenv("HW_LIVE_QUESTION") != "1" {
		t.Skip("answers the real claude binary's questions against a local API: set HW_LIVE_QUESTION=1 (no tokens)")
	}
	runQuestionScenarios(t, false)
}

func TestRecordQuestion(t *testing.T) {
	if os.Getenv("HW_RECORD_QUESTION") != "1" {
		t.Skip("records the question corpus: set HW_RECORD_QUESTION=1 (needs the real claude binary; no tokens)")
	}
	runQuestionScenarios(t, true)
}

// askedQuestion is one question of an AskUserQuestion call.
type askedQuestion struct {
	Question    string        `json:"question"`
	Header      string        `json:"header"`
	MultiSelect bool          `json:"multiSelect"`
	Options     []askedOption `json:"options"`
}

type askedOption struct {
	Label       string `json:"label"`
	Description string `json:"description"`
}

var (
	colourQuestion = askedQuestion{"Which colour do you prefer?", "Colour", false, []askedOption{{"Red", "You prefer red"}, {"Blue", "You prefer blue"}}}
	sizeQuestion   = askedQuestion{"Which size do you need?", "Size", false, []askedOption{{"Small", "A small one"}, {"Large", "A large one"}}}
	toppings       = askedQuestion{"Which toppings do you want?", "Toppings", true, []askedOption{{"Cheese", "Melted cheese"}, {"Mushrooms", "Sliced mushrooms"}, {"Olives", "Black olives"}}}
)

// questionScenario is one recording: claude asks questions, each request
// (kinds, in order) gets the answer at the same index, and claude's tool
// result must contain want.
type questionScenario struct {
	name      string
	notes     string
	questions []askedQuestion
	answers   []InputAnswer
	kinds     []string
	want      string
}

func runQuestionScenarios(t *testing.T, record bool) {
	bin, err := exec.LookPath("claude")
	if err != nil {
		t.Skipf("claude binary not on PATH: %v", err)
	}
	version, _ := exec.Command(bin, "--version").Output()
	q, review := claudecode.KindQuestion, claudecode.KindQuestionReview
	for _, sc := range []questionScenario{
		{
			name: "question-single", notes: "A single-select question answered with an option: its digit answers it.",
			questions: []askedQuestion{colourQuestion}, answers: []InputAnswer{{OptionID: "2"}},
			kinds: []string{q}, want: `"Which colour do you prefer?"="Blue"`,
		},
		{
			name: "question-other", notes: "A single-select question answered with text: the digit puts the highlight on \"Type something\", the text replaces its placeholder, Enter answers.",
			questions: []askedQuestion{colourQuestion}, answers: []InputAnswer{{Text: "Green"}},
			kinds: []string{q}, want: `"Which colour do you prefer?"="Green"`,
		},
		{
			name: "question-chat", notes: "\"Chat about this\": the digit declines the question, and claude goes on to ask what to clarify.",
			questions: []askedQuestion{colourQuestion}, answers: []InputAnswer{{OptionID: "chat"}},
			kinds: []string{q}, want: "The user wants to clarify these questions",
		},
		{
			name: "question-multi-select", notes: "A multi-select question: digits tick rows 1 and 3, Tab opens the review pane, whose digit submits.",
			questions: []askedQuestion{toppings}, answers: []InputAnswer{{OptionIDs: []string{"1", "3"}}, {OptionID: "proceed"}},
			kinds: []string{q, review}, want: `"Which toppings do you want?"="Cheese, Olives"`,
		},
		{
			name: "question-multi-select-other", notes: "A multi-select question with text: a digit ticks row 1, the arrows reach \"Type something\", the text replaces its placeholder, Down and Enter open the review pane.",
			questions: []askedQuestion{toppings}, answers: []InputAnswer{{OptionIDs: []string{"1", "other"}, Text: "Green"}, {OptionID: "proceed"}},
			kinds: []string{q, review}, want: `"Which toppings do you want?"="Cheese, Green"`,
		},
		{
			name: "question-two", notes: "Two questions in one call: each digit answers its question and moves on, then the review pane submits.",
			questions: []askedQuestion{colourQuestion, sizeQuestion}, answers: []InputAnswer{{OptionID: "2"}, {OptionID: "1"}, {OptionID: "proceed"}},
			kinds: []string{q, q, review}, want: `"Which colour do you prefer?"="Blue", "Which size do you need?"="Small"`,
		},
	} {
		t.Run(sc.name, func(t *testing.T) { runQuestionScenario(t, bin, string(version), sc, record) })
	}
}

func runQuestionScenario(t *testing.T, bin, version string, sc questionScenario, record bool) {
	results := make(chan string, 4)
	api := httptest.NewServer(questionAPI(sc.questions, results))
	defer api.Close()

	wd := filepath.Join("/tmp", "hw-rebake", sc.name)
	if err := os.RemoveAll(wd); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(wd, 0o755); err != nil {
		t.Fatal(err)
	}
	realWD, _ := filepath.EvalSymlinks(wd)
	cfg := t.TempDir()
	seed := func(file string, v any) {
		data, _ := json.Marshal(v)
		if err := os.WriteFile(filepath.Join(cfg, file), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	seed(".claude.json", map[string]any{
		"hasCompletedOnboarding":        true,
		"bypassPermissionsModeAccepted": true,
		"projects":                      map[string]any{realWD: map[string]any{"hasTrustDialogAccepted": true}},
	})
	seed("settings.json", map[string]any{
		"permissions":                       map[string]any{"defaultMode": "bypassPermissions"},
		"skipDangerousModePermissionPrompt": true,
	})

	conv, err := Open(context.Background(), Options{
		Harness:    harnessname.ClaudeCode,
		BinaryPath: bin,
		WorkingDir: wd,
		Env: append(harnessenv.Cleaned(),
			"CLAUDE_CONFIG_DIR="+cfg,
			"ANTHROPIC_BASE_URL="+api.URL,
			"ANTHROPIC_AUTH_TOKEN=placeholder-for-a-local-api"),
		Store:                     newFakeStore(),
		KeepAliveOnClassification: true,
		InputPolicy: &InputPolicy{ByKind: map[string]Disposition{
			"trust_prompt":      {Kind: DispositionAnswer, OptionID: "proceed"},
			"bypass_acceptance": {Kind: DispositionAnswer, OptionID: "proceed"},
		}},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = conv.Close(context.Background()) }()

	terminal := make(chan Turn, 1)
	var raisedMu sync.Mutex
	var raised []string
	go func() {
		for ev := range conv.Events() {
			if ev.Type == EventInputRequest {
				raisedMu.Lock()
				raised = append(raised, ev.Input.Kind)
				raisedMu.Unlock()
			}
			if ev.Type == EventTurn && ev.Turn.Role == RoleAssistant &&
				(ev.Turn.State == TurnStateComplete || ev.Turn.State == TurnStateErrored || ev.Turn.State == TurnStateInterrupted) {
				select {
				case terminal <- ev.Turn:
				default:
				}
			}
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	release, err := conv.AcquireControl(ctx)
	if err != nil {
		t.Fatalf("AcquireControl: %v", err)
	}
	if _, err := conv.Send(ctx, "HW_ASK: ask me"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	answered := ""
	for i, ans := range sc.answers {
		req := awaitNewPendingInput(t, conv, answered, 30*time.Second)
		answered = req.ID
		if req.Kind != sc.kinds[i] {
			t.Fatalf("request %d is a %q (%q), want a %q", i, req.Kind, req.Prompt, sc.kinds[i])
		}
		if err := conv.Answer(ctx, req.ID, ans); err != nil {
			t.Fatalf("Answer(%q, %+v): %v", req.Prompt, ans, err)
		}
	}
	release()
	select {
	case got := <-results:
		if !strings.Contains(got, sc.want) {
			t.Fatalf("claude reported %q, want it to contain %q", got, sc.want)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("claude sent no tool result within 30s")
	}
	select {
	case turn := <-terminal:
		if turn.State != TurnStateComplete || turn.Text != "GOT_ANSWER" {
			t.Fatalf("turn ended %s (%q): %q, want completed with the reply", turn.State, turn.Reason, turn.Text)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the turn did not end within 30s of the answer")
	}
	// Every request the session raised, phantoms included: a frame read while
	// claude was still painting a pane can raise one.
	raisedMu.Lock()
	got := strings.Join(raised, ",")
	raisedMu.Unlock()
	if got != strings.Join(sc.kinds, ",") {
		t.Errorf("the session raised %s, want exactly %s", got, strings.Join(sc.kinds, ","))
	}

	if !record {
		return
	}
	time.Sleep(time.Second) // let the last frame paint
	qctx, qcancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer qcancel()
	if err := conv.Quit(qctx); err != nil {
		t.Fatalf("Quit: %v", err)
	}
	_, _ = conv.Wrapper().Wait()
	raw := conv.Wrapper().RecentOutput()
	if len(raw) >= 64*1024 {
		t.Fatalf("the session wrote %d bytes, past the recent-output ring; the recording would be truncated", len(raw))
	}
	conv.mu.Lock()
	id := conv.session.HarnessID()
	conv.mu.Unlock()

	out := filepath.Join("..", "..", "test", "corpus", "claude-code", corpusName(sc.name))
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(out, "bytes.raw"), []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	meta, _ := json.MarshalIndent(map[string]any{
		"harness":        "claude-code",
		"binary_version": strings.Fields(version)[0],
		"recorded_at":    time.Now().UTC().Format(time.RFC3339Nano),
		"cols":           120,
		"rows":           40,
		"notes": sc.notes + platformNote() + " Recorded with `HW_RECORD_QUESTION=1 go test ./internal/chatcore -run RecordQuestion` " +
			"(internal/chatcore/question_record_test.go), answering through Conversation.Answer: the real claude against a " +
			"local Messages API that asks the question, in " + wd + " with a fresh CLAUDE_CONFIG_DIR (onboarding done, the " +
			"directory trusted, permissions.defaultMode bypassPermissions, accepted) and a placeholder ANTHROPIC_AUTH_TOKEN, so no account or tokens are involved.",
	}, "", " ")
	if err := os.WriteFile(filepath.Join(out, "meta.json"), append(meta, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cfg, "projects", transcriptcc.EncodedCWD(realWD), id+".jsonl")
	lines, err := reducedTranscript(path)
	if err != nil {
		t.Fatalf("transcript %s: %v", path, err)
	}
	if err := os.WriteFile(filepath.Join(out, "transcript.jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// awaitNewPendingInput waits for a pending request other than the one with
// id prev.
func awaitNewPendingInput(t *testing.T, conv *Conversation, prev string, within time.Duration) *InputRequest {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if p := conv.PendingInput(); p != nil && p.ID != prev {
			return p
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("no new input request within %s; the screen:\n%s", within, conv.ScreenSnapshot().Text)
	return nil
}

// questionAPI is the local Messages API the question scenarios run against:
// it asks questions with AskUserQuestion when the prompt says HW_ASK, sends
// claude's tool result to results, and answers it "GOT_ANSWER".
func questionAPI(questions []askedQuestion, results chan<- string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		stream := bytes.Contains(body, []byte(`"stream":true`))
		switch {
		case !strings.HasPrefix(r.URL.Path, "/v1/messages"):
			w.WriteHeader(http.StatusNotFound)
		case strings.Contains(r.URL.Path, "count_tokens"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"input_tokens":10}`)
		default:
			if res, ok := lastToolResult(body); ok {
				select {
				case results <- res:
				default:
				}
				writeLocalReply(w, stream, "GOT_ANSWER")
				return
			}
			if last, _ := lastUserText(body); stream && strings.Contains(last, "HW_ASK") && bytes.Contains(body, []byte(`"name":"AskUserQuestion"`)) {
				askQuestions(w, questions)
				return
			}
			writeLocalReply(w, stream, "ok")
		}
	}
}

// lastToolResult returns the text of the tool result in a Messages API
// request's last user message.
func lastToolResult(body []byte) (string, bool) {
	var req struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &req) != nil {
		return "", false
	}
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role != "user" {
			continue
		}
		var blocks []struct {
			Type    string          `json:"type"`
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(req.Messages[i].Content, &blocks) != nil {
			return "", false
		}
		for _, b := range blocks {
			if b.Type != "tool_result" {
				continue
			}
			var s string
			if json.Unmarshal(b.Content, &s) == nil {
				return s, true
			}
			var parts []struct {
				Text string `json:"text"`
			}
			_ = json.Unmarshal(b.Content, &parts)
			var out []string
			for _, p := range parts {
				out = append(out, p.Text)
			}
			return strings.Join(out, "\n"), true
		}
		return "", false
	}
	return "", false
}

// askQuestions answers with one AskUserQuestion tool call.
func askQuestions(w http.ResponseWriter, questions []askedQuestion) {
	input, _ := json.Marshal(map[string]any{"questions": questions})
	partial, _ := json.Marshal(string(input))
	event := sseWriter(w)
	event("message_start", `{"type":"message_start","message":{"id":"msg_local","type":"message","role":"assistant","model":"claude-local","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":1}}}`)
	event("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_ask1","name":"AskUserQuestion","input":{}}}`)
	event("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":`+string(partial)+`}}`)
	event("content_block_stop", `{"type":"content_block_stop","index":0}`)
	event("message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":20}}`)
	event("message_stop", `{"type":"message_stop"}`)
}

package wrapper

import (
	"io"
	"reflect"
	"testing"

	"github.com/olesho/harness-wrapper/pkg/wrapper/trace"
)

func TestArgsWithHarnessModel(t *testing.T) {
	tests := []struct {
		name    string
		harness string
		args    []string
		model   string
		want    []string
	}{
		{
			name:    "claude model prepended",
			harness: "claude",
			args:    []string{"-p", "prompt"},
			model:   "claude-opus-4-8",
			want:    []string{"--model", "claude-opus-4-8", "-p", "prompt"},
		},
		{
			// The chat layer passes "claude-code"; the bare switch would have
			// no-opped here before the harness-name normalization.
			name:    "claude-code name normalizes to claude",
			harness: "claude-code",
			args:    []string{"-p"},
			model:   "opus",
			want:    []string{"--model", "opus", "-p"},
		},
		{
			name:    "existing --model wins",
			harness: "claude",
			args:    []string{"--model", "sonnet", "-p"},
			model:   "opus",
			want:    []string{"--model", "sonnet", "-p"},
		},
		{
			name:    "codex model as config override",
			harness: "codex",
			args:    []string{"exec", "--json"},
			model:   "o3",
			want:    []string{"-c", "model=\"o3\"", "exec", "--json"},
		},
		{
			name:    "codex existing model wins",
			harness: "codex",
			args:    []string{"-c", "model=\"gpt\"", "exec"},
			model:   "o3",
			want:    []string{"-c", "model=\"gpt\"", "exec"},
		},
		{
			name:    "empty model leaves args unchanged",
			harness: "claude",
			args:    []string{"-p"},
			model:   "",
			want:    []string{"-p"},
		},
		{
			name:    "unsupported harness leaves args unchanged",
			harness: "opencode",
			args:    []string{"-p"},
			model:   "x",
			want:    []string{"-p"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := argsWithHarnessModel(tc.harness, tc.args, tc.model)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("argsWithHarnessModel() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestArgsWithHarnessMaxTokens(t *testing.T) {
	t.Run("codex cap as config override", func(t *testing.T) {
		got := argsWithHarnessMaxTokens("codex", []string{"exec"}, 32000, trace.Discard)
		want := []string{"-c", "model_max_output_tokens=32000", "exec"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("argsWithHarnessMaxTokens() = %v, want %v", got, want)
		}
	})
	t.Run("codex existing cap wins", func(t *testing.T) {
		args := []string{"-c", "model_max_output_tokens=10", "exec"}
		got := argsWithHarnessMaxTokens("codex", args, 32000, trace.Discard)
		if !reflect.DeepEqual(got, args) {
			t.Fatalf("argsWithHarnessMaxTokens() = %v, want %v", got, args)
		}
	})
	t.Run("zero leaves args unchanged", func(t *testing.T) {
		got := argsWithHarnessMaxTokens("codex", []string{"exec"}, 0, trace.Discard)
		if !reflect.DeepEqual(got, []string{"exec"}) {
			t.Fatalf("argsWithHarnessMaxTokens() = %v, want [exec]", got)
		}
	})
	t.Run("claude-code is a no-op plus an unsupported trace", func(t *testing.T) {
		cap := &captureEmitter{}
		got := argsWithHarnessMaxTokens("claude-code", []string{"-p"}, 32000, cap)
		if !reflect.DeepEqual(got, []string{"-p"}) {
			t.Fatalf("args changed for claude-code: %v", got)
		}
		if len(cap.events) != 1 || cap.events[0].Kind != "harness_token_cap_unsupported" {
			t.Fatalf("want one harness_token_cap_unsupported event, got %v", cap.events)
		}
	})
}

// TestHarnessNameNormalization guards the linchpin fix: the chat layer passes
// "claude-code", which must reach the same effort path as "claude".
func TestHarnessNameNormalization(t *testing.T) {
	if !harnessSupportsEffort("claude-code") {
		t.Fatal(`harnessSupportsEffort("claude-code") = false, want true`)
	}
	got := argsWithHarnessEffort("claude-code", []string{"-p"}, "high")
	want := []string{"--effort", "high", "-p"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("argsWithHarnessEffort(claude-code) = %v, want %v", got, want)
	}
}

// TestValidateConfig_ClaudeCodeEffort is the regression for the hard blocker:
// effort + the chat-layer harness name "claude-code" previously failed
// validation because harnessSupportsEffort only matched "claude".
func TestValidateConfig_ClaudeCodeEffort(t *testing.T) {
	err := validateConfig(&Config{BinaryPath: "x", Stdout: io.Discard, Harness: "claude-code", Effort: "high"})
	if err != nil {
		t.Fatalf("validateConfig() = %v, want nil", err)
	}
}

func TestValidateConfig_NegativeMaxTokens(t *testing.T) {
	err := validateConfig(&Config{BinaryPath: "x", Stdout: io.Discard, Harness: "codex", MaxTokens: -1})
	if err == nil {
		t.Fatal("validateConfig() = nil, want error for negative MaxTokens")
	}
}

type captureEmitter struct{ events []trace.Event }

func (c *captureEmitter) Emit(ev trace.Event) { c.events = append(c.events, ev) }

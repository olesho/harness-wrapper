package wrapcore

import (
	"errors"
	"io"
	"reflect"
	"testing"
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

// A Model for a harness argsWithHarnessModel has no flag for used to pass
// validation and then be dropped from the argv, so the harness ran on its
// default model while the caller believed it had chosen one. It is refused
// now, as an Effort on such a harness is.
func TestValidateConfig_ModelNeedsAModelFlag(t *testing.T) {
	for _, harness := range []string{"opencode", "pi", "cursor", "generic", ""} {
		err := validateConfig(&Config{BinaryPath: "x", Stdout: io.Discard, Harness: harness, Model: "gpt-5"})
		if !errors.Is(err, ErrInvalidConfig) {
			t.Errorf("harness %q with a Model: validateConfig() = %v, want ErrInvalidConfig", harness, err)
		}
		if _, err := HarnessArgs(Config{BinaryPath: "x", Harness: harness, Model: "gpt-5"}); !errors.Is(err, ErrInvalidConfig) {
			t.Errorf("harness %q with a Model: HarnessArgs() = %v, want ErrInvalidConfig", harness, err)
		}
		if err := validateConfig(&Config{BinaryPath: "x", Stdout: io.Discard, Harness: harness}); err != nil {
			t.Errorf("harness %q without a Model: validateConfig() = %v, want nil", harness, err)
		}
	}
	for _, harness := range []string{"claude", "claude-code", "codex", " Codex "} {
		if err := validateConfig(&Config{BinaryPath: "x", Stdout: io.Discard, Harness: harness, Model: "m"}); err != nil {
			t.Errorf("harness %q with a Model: validateConfig() = %v, want nil", harness, err)
		}
	}
}

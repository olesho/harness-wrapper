package wrapper

import (
	"reflect"
	"testing"
)

func TestArgsWithHarnessEffort(t *testing.T) {
	tests := []struct {
		name    string
		harness string
		args    []string
		effort  string
		want    []string
	}{
		{
			name:    "claude effort prepended",
			harness: "claude",
			args:    []string{"-p", "prompt"},
			effort:  "high",
			want:    []string{"--effort", "high", "-p", "prompt"},
		},
		{
			name:    "existing effort wins",
			harness: "claude",
			args:    []string{"--effort", "low", "-p", "prompt"},
			effort:  "high",
			want:    []string{"--effort", "low", "-p", "prompt"},
		},
		{
			name:    "empty effort leaves args unchanged",
			harness: "claude",
			args:    []string{"-p", "prompt"},
			effort:  "",
			want:    []string{"-p", "prompt"},
		},
		{
			name:    "codex effort prepended as config override",
			harness: "codex",
			args:    []string{"exec", "--json"},
			effort:  "high",
			want:    []string{"-c", "model_reasoning_effort=\"high\"", "exec", "--json"},
		},
		{
			name:    "codex max maps to xhigh",
			harness: "codex",
			args:    []string{"exec", "--json"},
			effort:  "max",
			want:    []string{"-c", "model_reasoning_effort=\"xhigh\"", "exec", "--json"},
		},
		{
			name:    "codex existing effort wins",
			harness: "codex",
			args:    []string{"exec", "-c", "model_reasoning_effort=\"low\"", "--json"},
			effort:  "high",
			want:    []string{"exec", "-c", "model_reasoning_effort=\"low\"", "--json"},
		},
		{
			name:    "unsupported harness leaves args unchanged",
			harness: "opencode",
			args:    []string{"-p", "prompt"},
			effort:  "high",
			want:    []string{"-p", "prompt"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := argsWithHarnessEffort(tc.harness, tc.args, tc.effort)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("argsWithHarnessEffort() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestIsSupportedEffort(t *testing.T) {
	for _, effort := range []string{"low", "medium", "high", "xhigh", "max"} {
		if !isSupportedEffort(effort) {
			t.Fatalf("isSupportedEffort(%q) = false, want true", effort)
		}
	}
	if isSupportedEffort("ultra") {
		t.Fatal("isSupportedEffort(\"ultra\") = true, want false")
	}
}

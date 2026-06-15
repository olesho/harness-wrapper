package wrapper

import (
	"os"
	"reflect"
	"strings"
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
			harness: "gemini",
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

func TestEnvWithHarnessEffort_GeminiSettingsPath(t *testing.T) {
	got := envWithHarnessEffort("gemini", []string{"FOO=bar"}, "high")
	settingsPath := envValue(got, "GEMINI_CLI_SYSTEM_SETTINGS_PATH")
	if settingsPath == "" {
		t.Fatalf("GEMINI_CLI_SYSTEM_SETTINGS_PATH missing from env: %v", got)
	}
	t.Cleanup(func() { _ = os.Remove(settingsPath) })
	body, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	if !strings.Contains(string(body), `"thinkingBudget": 8192`) {
		t.Fatalf("settings = %s, want thinkingBudget 8192", string(body))
	}
}

func TestEnvWithHarnessEffort_GeminiMaxSettingsPath(t *testing.T) {
	got := envWithHarnessEffort("gemini", []string{"FOO=bar"}, "max")
	settingsPath := envValue(got, "GEMINI_CLI_SYSTEM_SETTINGS_PATH")
	if settingsPath == "" {
		t.Fatalf("GEMINI_CLI_SYSTEM_SETTINGS_PATH missing from env: %v", got)
	}
	t.Cleanup(func() { _ = os.Remove(settingsPath) })
	body, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	if !strings.Contains(string(body), `"thinkingBudget": -1`) {
		t.Fatalf("settings = %s, want thinkingBudget -1", string(body))
	}
}

func TestEnvWithHarnessEffort_GeminiExistingSettingsPathWins(t *testing.T) {
	got := envWithHarnessEffort("gemini", []string{"GEMINI_CLI_SYSTEM_SETTINGS_PATH=/custom/settings.json"}, "high")
	if settingsPath := envValue(got, "GEMINI_CLI_SYSTEM_SETTINGS_PATH"); settingsPath != "/custom/settings.json" {
		t.Fatalf("settings path = %q, want /custom/settings.json", settingsPath)
	}
}

func envValue(env []string, key string) string {
	prefix := key + "="
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			return strings.TrimPrefix(entry, prefix)
		}
	}
	return ""
}

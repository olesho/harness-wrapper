package main

import (
	"reflect"
	"testing"
)

// TestApplySandboxDefaults freezes the injection policy: claude gets
// --dangerously-skip-permissions + IS_SANDBOX=1, other harnesses are a
// documented no-op, and both injections are idempotent against
// caller-supplied values (including the --flag=value spelling and exact-key
// env matching at the '=' boundary).
func TestApplySandboxDefaults(t *testing.T) {
	tests := []struct {
		name     string
		harness  string
		args     []string
		env      []string
		wantArgs []string
		wantEnv  []string
	}{
		{
			name:     "claude injects flag and env",
			harness:  "claude",
			args:     []string{"--model", "opus"},
			env:      []string{"PATH=/usr/bin"},
			wantArgs: []string{"--model", "opus", "--dangerously-skip-permissions"},
			wantEnv:  []string{"PATH=/usr/bin", "IS_SANDBOX=1"},
		},
		{
			name:     "codex is a no-op",
			harness:  "codex",
			args:     []string{"--model", "o3"},
			env:      []string{"PATH=/usr/bin"},
			wantArgs: []string{"--model", "o3"},
			wantEnv:  []string{"PATH=/usr/bin"},
		},
		{
			name:     "dedup exact flag token",
			harness:  "claude",
			args:     []string{"--dangerously-skip-permissions"},
			env:      []string{},
			wantArgs: []string{"--dangerously-skip-permissions"},
			wantEnv:  []string{"IS_SANDBOX=1"},
		},
		{
			name:     "dedup =value flag form",
			harness:  "claude",
			args:     []string{"--dangerously-skip-permissions=true"},
			env:      []string{},
			wantArgs: []string{"--dangerously-skip-permissions=true"},
			wantEnv:  []string{"IS_SANDBOX=1"},
		},
		{
			name:     "existing IS_SANDBOX value is left untouched",
			harness:  "claude",
			args:     []string{},
			env:      []string{"IS_SANDBOX=0"},
			wantArgs: []string{"--dangerously-skip-permissions"},
			wantEnv:  []string{"IS_SANDBOX=0"},
		},
		{
			name:     "prefix-sharing key does not suppress injection",
			harness:  "claude",
			args:     []string{},
			env:      []string{"IS_SANDBOXED=1"},
			wantArgs: []string{"--dangerously-skip-permissions"},
			wantEnv:  []string{"IS_SANDBOXED=1", "IS_SANDBOX=1"},
		},
		{
			name:     "empty inputs still inject for claude",
			harness:  "claude",
			args:     nil,
			env:      nil,
			wantArgs: []string{"--dangerously-skip-permissions"},
			wantEnv:  []string{"IS_SANDBOX=1"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotArgs, gotEnv := applySandboxDefaults(tt.harness, tt.args, tt.env)
			if !reflect.DeepEqual(gotArgs, tt.wantArgs) {
				t.Errorf("args = %v, want %v", gotArgs, tt.wantArgs)
			}
			if !reflect.DeepEqual(gotEnv, tt.wantEnv) {
				t.Errorf("env = %v, want %v", gotEnv, tt.wantEnv)
			}
		})
	}
}

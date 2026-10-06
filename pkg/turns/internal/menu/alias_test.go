package menu

import "testing"

func TestIntentAlias(t *testing.T) {
	cases := []struct{ label, want string }{
		// Real dialog rows (claude-code trust / bypass / permission, codex
		// trust / approval).
		{"Yes, I trust this folder", AliasProceed},
		{"No, exit", AliasDeny},
		{"Yes, I accept", AliasProceed},
		{"Yes, proceed", AliasProceed},
		{"Yes", AliasProceed},
		{"Yes, and don't ask again for this command", AliasProceed},
		{"No, and tell Claude what to do differently (esc)", AliasDeny},
		{"Yes, continue", AliasProceed},
		{"No, quit", AliasDeny},
		{"No", AliasDeny},
		// Negated proceed words must not read as proceed.
		{"No, I don't trust this folder", AliasDeny},
		{"Don't trust", AliasDeny},
		{"Do not continue", AliasDeny},
		// Unprefixed keywords.
		{"Trust this folder", AliasProceed},
		{"Continue", AliasProceed},
		{"Exit", AliasDeny},
		{"Cancel", AliasDeny},
		// Words that merely start with "no" are not a "No" answer.
		{"Notice", ""},
		{"Now", ""},
		{"Something else", ""},
	}
	for _, tc := range cases {
		if got := IntentAlias(tc.label); got != tc.want {
			t.Errorf("IntentAlias(%q) = %q, want %q", tc.label, got, tc.want)
		}
	}
}

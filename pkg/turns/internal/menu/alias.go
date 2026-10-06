// Package menu holds dialog-menu helpers shared by the screen-scraping turn
// adapters, so the claude-code and codex adapters cannot drift apart on how
// a menu row is interpreted.
package menu

import "strings"

// Intent aliases for menu options. Input policies target these instead of
// harness-specific wording.
const (
	AliasProceed = "proceed"
	AliasDeny    = "deny"
)

// IntentAlias maps a menu label to AliasProceed, AliasDeny, or "".
//
// Dialog rows lead with their answer ("Yes, I trust this folder", "No, exit",
// "Yes, and don't ask again for this command"), so the leading word decides
// first. Only an unprefixed label falls back to keywords, where a negated
// proceed verb ("Don't trust", "Do not continue") is a deny. Checking the
// proceed keywords first would map "No, I don't trust this folder" to
// proceed because it contains "trust"; checking the deny keywords first would
// map "Yes, and don't ask again" to deny because it contains "don't".
func IntentAlias(label string) string {
	l := strings.ToLower(strings.TrimSpace(label))
	switch firstWord(l) {
	case "yes":
		return AliasProceed
	case "no":
		return AliasDeny
	}
	for _, neg := range []string{"don't ", "do not ", "not "} {
		for _, verb := range proceedWords {
			if strings.Contains(l, neg+verb) {
				return AliasDeny
			}
		}
	}
	switch {
	case containsAny(l, proceedWords...):
		return AliasProceed
	case containsAny(l, "exit", "deny", "reject", "cancel", "quit", "don't", "do not"):
		return AliasDeny
	default:
		return ""
	}
}

var proceedWords = []string{"proceed", "accept", "trust", "continue", "allow", "approve"}

// firstWord returns l's leading run of letters ("no" for "no, exit").
func firstWord(l string) string {
	end := strings.IndexFunc(l, func(r rune) bool { return r < 'a' || r > 'z' })
	if end < 0 {
		return l
	}
	return l[:end]
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

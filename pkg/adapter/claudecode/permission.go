package claudecode

import (
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/olesho/harness-wrapper/pkg/contract"
)

// A permission claude asks for at the gated posture is a prompt of kind
// "permission" whose options are claude's own three: run the tool once
// (yes), run it and keep the rule claude suggests so it is not asked again
// (always, offered only when claude suggests one), or refuse it (no), which
// the model is told and goes on from. Both Claude profiles raise it: the
// stream-json transport for a can_use_tool control request, the TUI's for
// its PermissionRequest hook.

// PromptKindPermission is the kind of the prompts claude raises.
const PromptKindPermission = "permission"

// The options of a permission prompt.
const (
	OptionYes    = "yes"
	OptionAlways = "always"
	OptionNo     = "no"
)

// DeniedMessage is what the model is told of a tool the prompt refused.
const DeniedMessage = "The user denied permission to use this tool."

// maxPromptText bounds a prompt's text.
const maxPromptText = 2 << 10

// Permission is one permission claude asks for.
type Permission struct {
	Tool string
	// Input is the tool's input, as claude gave it.
	Input json.RawMessage
	// Suggestions are the rules claude suggests keeping for "always"
	// (permission_suggestions).
	Suggestions json.RawMessage
	// NoAlways: claude offers no rule to keep (suppress_always_allow_rule).
	NoAlways bool
}

// canAlways reports whether the permission offers "always": claude suggests
// a rule to keep.
func (p Permission) canAlways() bool {
	if p.NoAlways {
		return false
	}
	var s []json.RawMessage
	return json.Unmarshal(p.Suggestions, &s) == nil && len(s) > 0
}

// Prompt is the permission as the contract's prompt promptID.
func (p Permission) Prompt(promptID string) contract.PromptInfo {
	opts := []contract.PromptOption{{OptionID: OptionYes, Label: "Yes"}}
	if p.canAlways() {
		opts = append(opts, contract.PromptOption{OptionID: OptionAlways, Label: "Yes, and don't ask again"})
	}
	opts = append(opts, contract.PromptOption{OptionID: OptionNo, Label: "No"})
	return contract.PromptInfo{PromptID: promptID, Kind: PromptKindPermission, Prompt: p.text(), Options: opts}
}

// text says what the tool would do: its command or file, or else its input.
func (p Permission) text() string {
	var in map[string]any
	_ = json.Unmarshal(p.Input, &in)
	what := ""
	for _, k := range []string{"command", "file_path", "notebook_path", "path", "url", "pattern"} {
		if v, ok := in[k].(string); ok && v != "" {
			what = v
			break
		}
	}
	if what == "" && len(p.Input) > 0 && string(p.Input) != "null" {
		what = string(p.Input)
	}
	text := "Allow " + p.Tool + "?"
	if what != "" {
		text = "Allow " + p.Tool + ": " + what + "?"
	}
	return clip(text, maxPromptText)
}

// clip cuts s to at most n bytes, on a rune boundary, marking the cut.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n - len("…")
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return strings.TrimSpace(s[:cut]) + "…"
}

// Decision is claude's answer to the permission for choice c: allow, with the
// input unchanged and, for "always", the rules claude suggested; or deny,
// with DeniedMessage. classify adds claude's decisionClassification, which
// the control protocol takes and a hook's output does not.
func (p Permission) Decision(c contract.Choice, classify bool) map[string]any {
	d := map[string]any{}
	switch c.OptionID {
	case OptionYes, OptionAlways:
		d["behavior"] = "allow"
		if len(p.Input) > 0 && string(p.Input) != "null" {
			d["updatedInput"] = p.Input
		}
		if c.OptionID == OptionAlways && p.canAlways() {
			d["updatedPermissions"] = p.Suggestions
		}
	default:
		d["behavior"] = "deny"
		d["message"] = DeniedMessage
	}
	if classify {
		d["decisionClassification"] = map[string]string{
			OptionYes: "user_temporary", OptionAlways: "user_permanent",
		}[c.OptionID]
		if d["decisionClassification"] == "" {
			d["decisionClassification"] = "user_reject"
		}
	}
	return d
}

// NoOneToAsk is the decision for a permission no input's turn is there to
// ask in — a turn of claude's own: denied, since nothing can answer it there
// and claude would wait for ever.
func NoOneToAsk() map[string]any {
	return map[string]any{"behavior": "deny", "message": "No one can answer a permission request outside a turn the user started."}
}

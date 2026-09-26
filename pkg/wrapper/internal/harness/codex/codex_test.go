package codex

import (
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

func TestMatchAPIError(t *testing.T) {
	cases := []struct {
		name        string
		in          string
		wantOK      bool
		wantCode    int
		msgContains string
	}{
		{
			name:        "Cx1: explicit retry-limit with 429",
			in:          "■ exceeded retry limit, last status: 429 Too Many Requests",
			wantOK:      true,
			wantCode:    429,
			msgContains: "retry limit",
		},
		{
			name:        "Cx2: capacity phrase → 503",
			in:          "ERROR: Selected model is at capacity. Please try a different model.",
			wantOK:      true,
			wantCode:    503,
			msgContains: "capacity",
		},
		{
			name:        "Cx3: high-demand phrase → 500",
			in:          "■ We're currently experiencing high demand, which may cause temporary errors.",
			wantOK:      true,
			wantCode:    500,
			msgContains: "high demand",
		},
		{
			name:        "Cx4: usage-limit phrase → 429",
			in:          "■ Usage limit reached. Try again at 14:00 UTC.",
			wantOK:      true,
			wantCode:    429,
			msgContains: "Usage limit",
		},
		{
			name:        "Cx5: quota phrase → 429",
			in:          "■ Quota exceeded. Check your plan and billing details.",
			wantOK:      true,
			wantCode:    429,
			msgContains: "Quota",
		},
		{
			name:        "Cx6: stream disconnected → code 0",
			in:          "■ stream disconnected before completion: connection reset",
			wantOK:      true,
			wantCode:    0,
			msgContains: "stream disconnected",
		},
		{
			name:   "Cx7: benign tool output",
			in:     "■ regular tool output",
			wantOK: false,
		},
		{
			name:   "Cx8: ERROR prefix alone is not a signal",
			in:     "ERROR: filesystem permission denied",
			wantOK: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hit, ok := MatchAPIError(tc.in)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (hit=%+v)", ok, tc.wantOK, hit)
			}
			if !ok {
				return
			}
			if hit.Code != tc.wantCode {
				t.Errorf("Code = %d, want %d", hit.Code, tc.wantCode)
			}
			if tc.msgContains != "" && !strings.Contains(strings.ToLower(hit.Message), strings.ToLower(tc.msgContains)) {
				t.Errorf("Message = %q, want substring %q", hit.Message, tc.msgContains)
			}
		})
	}
}

// TestMatchAPIErrorUnicodeBeforePhrase is the regression for a panic: the
// phrase used to be found in strings.ToLower(stripped) and its offset used to
// slice stripped itself. Lower-casing changes the UTF-8 length of some runes,
// so any such rune ahead of the phrase shifted the offset: 50 × "Ⱥ" (two bytes,
// "ⱥ" is three) put it past the end of the output, a slice-bounds panic on the
// classifier goroutine that ended the host process; "İ" (two bytes, "i" is
// one) cut the wrong text.
func TestMatchAPIErrorUnicodeBeforePhrase(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"grows when lower-cased", strings.Repeat("Ⱥ", 50) + "usage limit reached", "usage limit reached"},
		{"shrinks when lower-cased", strings.Repeat("İ", 50) + "■ Usage limit reached. Try again at 14:00 UTC.", "Usage limit reached"},
		{"mixed", "ẞ Ⱦİ K ■ Selected model is at capacity.", "Selected model is at capacity"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hit, ok := MatchAPIError(tc.in)
			if !ok {
				t.Fatalf("no hit for %q", tc.in)
			}
			if hit.Message != tc.want {
				t.Fatalf("Message = %q, want %q", hit.Message, tc.want)
			}
		})
	}

	// Every rune whose lower-case form has another UTF-8 length.
	var n int
	for r := rune(0x80); r <= unicode.MaxRune; r++ {
		s := string(r)
		if len(strings.ToLower(s)) == len(s) {
			continue
		}
		n++
		hit, ok := MatchAPIError(strings.Repeat(s, 3) + "■ Quota exceeded. Check your plan.")
		if !ok || hit.Code != 429 || hit.Message != "Quota exceeded" {
			t.Fatalf("%U before the phrase: hit=%+v ok=%v, want the 429 hit with Message %q", r, hit, ok, "Quota exceeded")
		}
	}
	if n == 0 {
		t.Fatal("found no rune that changes length when lower-cased; the check above tested nothing")
	}
}

// The phrase table is matched against asciiLower of the output, which only
// folds ASCII letters: a phrase with an upper-case or non-ASCII letter could
// never match.
func TestCodexPhrasesAreLowerASCII(t *testing.T) {
	for _, p := range codexPhraseHits {
		for i := 0; i < len(p.Phrase); i++ {
			if c := p.Phrase[i]; c >= utf8.RuneSelf || ('A' <= c && c <= 'Z') {
				t.Errorf("phrase %q is not lower-case ASCII", p.Phrase)
				break
			}
		}
	}
}

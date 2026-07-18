package claude

import (
	"strings"
	"testing"
	"time"
)

// apiErrorCase is one row of the MatchAPIError table test.
type apiErrorCase struct {
	name        string
	in          string
	wantOK      bool
	wantCode    int
	wantRetry   time.Duration
	msgContains string
}

func TestMatchAPIError(t *testing.T) {
	cases := []apiErrorCase{
		{
			name:        "Cl1: golden 529 from user's transcript",
			in:          "API Error: 529 Overloaded. This is a server-side issue, usually temporary — try again in a moment.",
			wantOK:      true,
			wantCode:    529,
			msgContains: "Overloaded",
		},
		{
			name:      "Cl2: 429 with numeric retry-after",
			in:        "API Error: 429 Too Many Requests. Retry after 30 seconds.",
			wantOK:    true,
			wantCode:  429,
			wantRetry: 30 * time.Second,
		},
		{
			name:      "Cl3: 503 with minutes unit",
			in:        "API Error: 503 Service Unavailable. Try again in 2 minutes.",
			wantOK:    true,
			wantCode:  503,
			wantRetry: 2 * time.Minute,
		},
		{
			name:        "Cl4: 500 no retry hint",
			in:          "API Error: 500 Internal Server Error.",
			wantOK:      true,
			wantCode:    500,
			msgContains: "Internal Server Error",
		},
		{
			name:        "Cl5: 502 no trailing punctuation",
			in:          "API Error: 502 Bad Gateway",
			wantOK:      true,
			wantCode:    502,
			msgContains: "Bad Gateway",
		},
		{
			name:     "Cl6: leading whitespace tolerated",
			in:       "  API Error: 529 Overloaded",
			wantOK:   true,
			wantCode: 529,
		},
		{
			name:   "Cl7: mid-line user prompt rejected",
			in:     "What does API Error: 500 mean?",
			wantOK: false,
		},
		{
			name:     "Cl8: matches non-final line",
			in:       "previous output\nAPI Error: 529 Overloaded\nmore output",
			wantOK:   true,
			wantCode: 529,
		},
		{
			name:   "Cl9: 4-digit code rejected",
			in:     "API Error: 9999 unrecognized",
			wantOK: false,
		},
		{
			name:     "Cl10: lowercase variant",
			in:       "api error: 529 overloaded",
			wantOK:   true,
			wantCode: 529,
		},
		{
			name:   "Cl11: empty string",
			in:     "",
			wantOK: false,
		},
		{
			name:        "Cl12: 401 Unauthorized still matches",
			in:          "API Error: 401 Unauthorized.",
			wantOK:      true,
			wantCode:    401,
			msgContains: "Unauthorized",
		},
		{
			name:     "Cl13: repeated lines return first hit (not loop)",
			in:       "API Error: 529 Overloaded.\nAPI Error: 529 Overloaded.\nAPI Error: 529 Overloaded.",
			wantOK:   true,
			wantCode: 529,
		},
		{
			name:        "Cl14: transport error (no code) with tree-character prefix",
			in:          "  ⎿  API Error: The socket connection was closed unexpectedly. For more information, pass `verbose: true` in the second argument to fetch()",
			wantOK:      true,
			wantCode:    0,
			msgContains: "socket connection was closed unexpectedly",
		},
		{
			name:     "Cl15: tree-character prefix with code",
			in:       "  ⎿  API Error: 529 Overloaded.",
			wantOK:   true,
			wantCode: 529,
		},
		{
			name:        "Cl16: bare uncoded form",
			in:          "API Error: Connection refused.",
			wantOK:      true,
			wantCode:    0,
			msgContains: "Connection refused",
		},
		{
			name:      "Cl17: uncoded with retry hint",
			in:        "   API Error: Connection reset. Please retry in 30 seconds.",
			wantOK:    true,
			wantCode:  0,
			wantRetry: 30 * time.Second,
		},
		{
			name:        "Cl18: 500 server error with tree-character NBSP prefix",
			in:          "  ⎿  API Error: 500 Internal server error. This is a server-side issue, usually temporary — try again in a moment. If it persists, check status.claude.com.",
			wantOK:      true,
			wantCode:    500,
			msgContains: "Internal server error",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			checkAPIErrorCase(t, tc)
		})
	}
}

// checkAPIErrorCase runs MatchAPIError for one case and asserts the result.
func checkAPIErrorCase(t *testing.T, tc apiErrorCase) {
	t.Helper()
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
	if hit.RetryAfter != tc.wantRetry {
		t.Errorf("RetryAfter = %v, want %v", hit.RetryAfter, tc.wantRetry)
	}
	if tc.msgContains != "" && !strings.Contains(hit.Message, tc.msgContains) {
		t.Errorf("Message = %q, want substring %q", hit.Message, tc.msgContains)
	}
}

func TestMatchSessionLimit(t *testing.T) {
	warsaw, err := time.LoadLocation("Europe/Warsaw")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	now := time.Date(2026, 5, 20, 14, 0, 0, 0, warsaw)

	cases := []sessionLimitCase{
		{
			// Golden case from the user's transcript: a tool-result
			// frame ("⎿") wrapping the banner, followed by the
			// /usage-credits hint on the next visual line.
			name:        "SL1: verbatim golden — tool-result frame, Europe/Warsaw",
			in:          "  ⎿  You've hit your session limit · resets 6:40pm (Europe/Warsaw)\n     /usage-credits to finish what you're working on.",
			wantOK:      true,
			wantResume:  time.Date(2026, 5, 20, 18, 40, 0, 0, warsaw),
			msgContains: "session limit",
		},
		{
			name:        "SL2: no decoration prefix, plain line",
			in:          "You've hit your session limit · resets 8pm (UTC)",
			wantOK:      true,
			wantResume:  time.Date(2026, 5, 20, 20, 0, 0, 0, time.UTC),
			msgContains: "session limit",
		},
		{
			name:        "SL3: 'usage limit' phrasing also matches",
			in:          "⎿ You've hit your usage limit · resets 9:15am (UTC)",
			wantOK:      true,
			wantResume:  time.Date(2026, 5, 21, 9, 15, 0, 0, time.UTC),
			msgContains: "usage limit",
		},
		{
			name:        "SL4: contracted form 'you have'",
			in:          "⎿ You have hit your session limit · resets 6:40pm (Europe/Warsaw)",
			wantOK:      true,
			wantResume:  time.Date(2026, 5, 20, 18, 40, 0, 0, warsaw),
			msgContains: "session limit",
		},
		{
			name:         "SL5: banner without resets clause still matches (no ResumeAt)",
			in:           "  ⎿  You've hit your session limit. Try again later.",
			wantOK:       true,
			resumeIsZero: true,
			msgContains:  "session limit",
		},
		{
			name:   "SL6: false positive — assistant prose mid-line is rejected",
			in:     "the docs say you've hit your session limit when N tokens are reached",
			wantOK: false,
		},
		{
			name:   "SL7: empty input",
			in:     "",
			wantOK: false,
		},
		{
			name:   "SL8: unrelated rate-limit phrasing does not match (Cost path catches it)",
			in:     "rate limit exceeded",
			wantOK: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			checkSessionLimitCase(t, tc, now)
		})
	}
}

// sessionLimitCase is one row of the MatchSessionLimit table test.
type sessionLimitCase struct {
	name         string
	in           string
	wantOK       bool
	wantResume   time.Time
	msgContains  string
	resumeIsZero bool
}

// checkSessionLimitCase runs MatchSessionLimit for one case and asserts
// the result against the `now` clock.
func checkSessionLimitCase(t *testing.T, tc sessionLimitCase, now time.Time) {
	t.Helper()
	hit, ok := MatchSessionLimit(tc.in, now)
	if ok != tc.wantOK {
		t.Fatalf("ok = %v, want %v (hit=%+v)", ok, tc.wantOK, hit)
	}
	if !ok {
		return
	}
	if tc.msgContains != "" && !strings.Contains(strings.ToLower(hit.Message), tc.msgContains) {
		t.Errorf("Message = %q, want substring %q", hit.Message, tc.msgContains)
	}
	if tc.resumeIsZero {
		if !hit.ResumeAt.IsZero() {
			t.Errorf("ResumeAt = %s, want zero", hit.ResumeAt)
		}
		return
	}
	if !hit.ResumeAt.Equal(tc.wantResume) {
		t.Errorf("ResumeAt = %s, want %s", hit.ResumeAt, tc.wantResume)
	}
}

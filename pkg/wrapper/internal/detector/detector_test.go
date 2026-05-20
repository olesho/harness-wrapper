package detector

import (
	"testing"
	"time"
)

func mustLoad(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("LoadLocation(%q): %v", name, err)
	}
	return loc
}

func TestParseRetryAfter(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want time.Duration
	}{
		{"H1: numeric seconds spelled out", "Retry after 30 seconds.", 30 * time.Second},
		{"H2: numeric minutes spelled out", "Try again in 2 minutes.", 2 * time.Minute},
		{"H3: non-numeric phrasing", "try again in a moment", 0},
		{"H4: empty string", "", 0},
		{"H5: compact unit", "retry after 5s", 5 * time.Second},
		{"H6: no numeric hint", "please try again later", 0},
		{"hours unit", "try again in 1 hour", time.Hour},
		{"mid-message minutes", "the upstream said try again in 10 minutes please", 10 * time.Minute},
		{"zero rejected", "try again in 0 seconds", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseRetryAfter(tc.in)
			if got != tc.want {
				t.Errorf("ParseRetryAfter(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestParseResetTime(t *testing.T) {
	warsaw := mustLoad(t, "Europe/Warsaw")
	la := mustLoad(t, "America/Los_Angeles")

	// Anchor: 2026-05-20 14:00 in Europe/Warsaw — the time captured in
	// CLAUDE.md as today's date, with a wall-clock chosen so the golden
	// "6:40pm" target is later the same day.
	nowAfternoon := time.Date(2026, 5, 20, 14, 0, 0, 0, warsaw)
	nowEvening := time.Date(2026, 5, 20, 22, 0, 0, 0, warsaw)

	cases := []struct {
		name   string
		in     string
		now    time.Time
		wantOK bool
		want   time.Time
	}{
		{
			name:   "R1: verbatim Claude Code banner — same-day future",
			in:     "You've hit your session limit · resets 6:40pm (Europe/Warsaw)",
			now:    nowAfternoon,
			wantOK: true,
			want:   time.Date(2026, 5, 20, 18, 40, 0, 0, warsaw),
		},
		{
			name:   "R2: same banner past today rolls to tomorrow",
			in:     "You've hit your session limit · resets 6:40pm (Europe/Warsaw)",
			now:    nowEvening,
			wantOK: true,
			want:   time.Date(2026, 5, 21, 18, 40, 0, 0, warsaw),
		},
		{
			name:   "R3: 12pm (noon) resolves to 12:00, not 24:00",
			in:     "resets 12pm (UTC)",
			now:    time.Date(2026, 5, 20, 9, 0, 0, 0, time.UTC),
			wantOK: true,
			want:   time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC),
		},
		{
			name:   "R4: 12am (midnight) resolves to 00:00 next day",
			in:     "resets 12am (UTC)",
			now:    time.Date(2026, 5, 20, 9, 0, 0, 0, time.UTC),
			wantOK: true,
			want:   time.Date(2026, 5, 21, 0, 0, 0, 0, time.UTC),
		},
		{
			name:   "R5: no TZ — resolves in caller's location",
			in:     "resets 5pm",
			now:    time.Date(2026, 5, 20, 9, 0, 0, 0, la),
			wantOK: true,
			want:   time.Date(2026, 5, 20, 17, 0, 0, 0, la),
		},
		{
			name:   "R6: 24-hour with minutes",
			in:     "resets 18:40 (Europe/Warsaw)",
			now:    nowAfternoon,
			wantOK: true,
			want:   time.Date(2026, 5, 20, 18, 40, 0, 0, warsaw),
		},
		{
			name:   "R7: 'resets at' phrasing still matches",
			in:     "limit resets at 6:40pm (Europe/Warsaw)",
			now:    nowAfternoon,
			wantOK: true,
			want:   time.Date(2026, 5, 20, 18, 40, 0, 0, warsaw),
		},
		{
			name:   "R8: unrecognized TZ falls back to caller location",
			in:     "resets 6:40pm (Atlantis/Lemuria)",
			now:    nowAfternoon,
			wantOK: true,
			want:   time.Date(2026, 5, 20, 18, 40, 0, 0, warsaw),
		},
		{
			name:   "R9: no clock-time → no match",
			in:     "limit resets soon",
			now:    nowAfternoon,
			wantOK: false,
		},
		{
			name:   "R10: bare single-digit 24h hour is rejected (false-positive guard)",
			in:     "resets 6 (UTC)",
			now:    nowAfternoon,
			wantOK: false,
		},
		{
			name:   "R11: out-of-range minutes → no match",
			in:     "resets 6:99pm (UTC)",
			now:    nowAfternoon,
			wantOK: false,
		},
		{
			name:   "R12: empty string",
			in:     "",
			now:    nowAfternoon,
			wantOK: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ParseResetTime(tc.in, tc.now)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (got=%v)", ok, tc.wantOK, got)
			}
			if !ok {
				return
			}
			if !got.Equal(tc.want) {
				t.Errorf("ParseResetTime(%q) = %s, want %s", tc.in, got, tc.want)
			}
			// Returned time must be strictly in the future of `now`.
			if !got.After(tc.now) {
				t.Errorf("ParseResetTime returned non-future time: got=%s, now=%s", got, tc.now)
			}
		})
	}
}

package mockapi

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestStallShorterThanASecondEndsOnTime(t *testing.T) {
	s := &Server{}
	rec := httptest.NewRecorder()
	start := time.Now()
	s.reply(rec, body{Stream: true}, reply{text: "after stall", stall: 150 * time.Millisecond})
	if took := time.Since(start); took < 150*time.Millisecond || took > 900*time.Millisecond {
		t.Fatalf("a 150ms stall took %v", took)
	}
	if !strings.Contains(rec.Body.String(), "event: ping") {
		t.Error("stall sent no ping")
	}
}

// HUSH answers nothing, not even the response's status, until its time is
// up; then it replies.
func TestHushAnswersNothingUntilItsTime(t *testing.T) {
	s := Start()
	defer s.Close()
	start := time.Now()
	resp := post(t, s, "HUSH 1", true)
	if took := time.Since(start); took < time.Second {
		t.Fatalf("HUSH 1 answered after %v", took)
	}
	if got := deltas(t, resp); got != "after hush" {
		t.Errorf("HUSH 1 replied %q", got)
	}
}

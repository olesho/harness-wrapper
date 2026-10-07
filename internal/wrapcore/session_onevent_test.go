package wrapcore

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/olesho/harness-wrapper/internal/delivery"
)

func TestOversizedStatusEventIsTruncatedNotDropped(t *testing.T) {
	var got []SessionEvent
	q := delivery.New(delivery.Limits{Bytes: 512}, sessionEventSize, func(e SessionEvent) {
		got = append(got, e)
	})
	s := &Session{
		onEvent:     q,
		events:      make(chan SessionEvent, 16),
		stopRequest: make(chan struct{}),
	}
	long := strings.Repeat("é", 1000) // 2000 bytes, over the bound
	s.recordStatusChange(classification{status: StatusAPIError, reason: long}, false)
	q.Close()
	<-q.Drained()

	if len(got) != 1 {
		t.Fatalf("OnEvent got %d events, want 1", len(got))
	}
	r := got[0].Reason
	if got[0].Status != StatusAPIError {
		t.Errorf("status = %v, want %v", got[0].Status, StatusAPIError)
	}
	if !strings.HasSuffix(r, truncatedMark) || !strings.HasPrefix(r, "éé") {
		t.Errorf("reason not truncated with the mark: %q", r)
	}
	if !utf8.ValidString(r) {
		t.Error("truncated reason is not valid UTF-8")
	}
	if n := sessionEventSize(got[0]); n > 512 {
		t.Errorf("delivered event is %d bytes, over the 512-byte bound", n)
	}
	if st := q.Stats(); st.Undelivered != 0 {
		t.Errorf("undelivered = %d, want 0", st.Undelivered)
	}
}

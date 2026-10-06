package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// flushProbe records, at the first Flush (the headers going out), whether the
// fan-out already had a subscriber, and then ends the request.
type flushProbe struct {
	*httptest.ResponseRecorder
	fan        *fanout
	cancel     context.CancelFunc
	flushed    bool
	subscribed bool
}

func (p *flushProbe) Flush() {
	if !p.flushed {
		p.flushed = true
		p.fan.mu.Lock()
		p.subscribed = len(p.fan.subscribers) > 0
		p.fan.mu.Unlock()
		p.cancel()
	}
	p.ResponseRecorder.Flush()
}

// TestStreamEvents_SubscribedBeforeHeaders: a client may take the 200 on GET
// /events as "the stream is live" and Send at once, and events are not
// replayed. The fan-out subscription must therefore exist by the time the
// headers are flushed, not be taken after.
func TestStreamEvents_SubscribedBeforeHeaders(t *testing.T) {
	s := NewServer()
	fan := newFanout()
	s.mu.Lock()
	s.convs["c1"] = &convEntry{id: "c1", fan: fan}
	s.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/v1/conversations/c1/events", nil).WithContext(ctx)
	probe := &flushProbe{ResponseRecorder: httptest.NewRecorder(), fan: fan, cancel: cancel}
	s.Routes().ServeHTTP(probe, req)

	if probe.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", probe.Code)
	}
	if !probe.flushed {
		t.Fatal("precondition: the handler never flushed its headers")
	}
	if !probe.subscribed {
		t.Fatal("headers were flushed before the fan-out subscription existed; an event published as the client sees them is lost")
	}
}

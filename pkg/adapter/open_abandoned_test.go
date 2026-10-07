package adapter

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/pkg/contract"
)

// An Open whose ctx ends — the caller cancels it or its deadline passes —
// fails open_failed with no reason. Nothing about the config failed, so it
// must not read as config_invalid; the caller owns ctx and knows which.
func TestOpenAbandonedByCtxClaimsNoReason(t *testing.T) {
	for name, mk := range map[string]func() (context.Context, context.CancelFunc){
		"cancelled": func() (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return ctx, func() {}
		},
		"deadline": func() (context.Context, context.CancelFunc) {
			return context.WithTimeout(context.Background(), time.Millisecond)
		},
	} {
		t.Run(name, func(t *testing.T) {
			ag := fakeAgent(t)
			s, err := ag.a.NewSession(contract.OpenRequest{Mode: contract.OpenFresh, OpenConfig: ag.oc, Layout: ag.layout})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := mk()
			defer cancel()
			_, err = s.Open(ctx)
			var ce *contract.Error
			if !errors.As(err, &ce) {
				t.Fatalf("Open = %v, want a *contract.Error", err)
			}
			if ce.Code != contract.CodeOpenFailed || ce.Reason != "" || !strings.HasPrefix(ce.Message, "abandoned: ") {
				t.Fatalf("Open = %+v, want open_failed with no reason, \"abandoned: …\"", *ce)
			}
		})
	}
}

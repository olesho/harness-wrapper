// harness-chatd exposes pkg/chat over HTTP + SSE so non-Go clients
// (Python, TypeScript, …) can drive multi-turn harness conversations
// across a process boundary.
//
//	harness-chatd [--bind 127.0.0.1:8080] [--allow-remote]
//
// v1 has no auth: it refuses a non-loopback --bind, a Host that is not
// loopback, and every request a browser sends (see guard.go). --allow-remote
// lifts the first two for a server that something else protects. See
// clients/ for reference clients.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// chatdFlagSet registers harness-chatd's CLI flags onto a fresh FlagSet and
// returns it alongside the bound --bind target. Single definition of the flag
// surface so the contract test can enumerate it (see contract_test.go).
func chatdFlagSet() (*flag.FlagSet, *chatdFlags) {
	fs := flag.NewFlagSet("harness-chatd", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	f := &chatdFlags{
		bind:        fs.String("bind", "127.0.0.1:8080", "host:port to listen on"),
		allowRemote: fs.Bool("allow-remote", false, "accept a non-loopback --bind and any Host header; v1 has no auth, so only behind something that authenticates"),
	}
	return fs, f
}

type chatdFlags struct {
	bind        *string
	allowRemote *bool
}

func run(args []string) error {
	fs, flags := chatdFlagSet()
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("harness-chatd: %w", err)
	}
	bind := flags.bind
	if *flags.allowRemote {
		log.Printf("harness-chatd: --allow-remote: no auth and no Host check; anyone who reaches %s can run any binary", *bind)
	} else if err := requireLoopbackBind(*bind); err != nil {
		return fmt.Errorf("harness-chatd: %w", err)
	}

	srv := NewServer()
	srv.allowRemote = *flags.allowRemote
	httpSrv := &http.Server{
		Addr:              *bind,
		Handler:           srv.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		log.Printf("harness-chatd: listening on %s", *bind)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		log.Printf("harness-chatd: shutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	srv.Shutdown(shutdownCtx)
	return httpSrv.Shutdown(shutdownCtx)
}

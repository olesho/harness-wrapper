// harness-chatd exposes pkg/chat over HTTP + SSE so non-Go clients
// (Python, TypeScript, …) can drive multi-turn harness conversations
// across a process boundary.
//
//	harness-chatd [--bind 127.0.0.1:8080]
//
// v1 has no auth; bind to localhost. See clients/ for reference clients.
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

func run(args []string) error {
	fs := flag.NewFlagSet("harness-chatd", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	bind := fs.String("bind", "127.0.0.1:8080", "host:port to listen on")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("harness-chatd: %w", err)
	}

	srv := NewServer()
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

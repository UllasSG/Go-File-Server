// Command gofs serves a JSON HTTP API over a single directory tree.
//
// Rung 0 scope: flags, a health check, and graceful shutdown. No database
// and no filesystem access yet — those arrive in later rungs.
//
// main does two things only: install signal handling and turn run's error
// into an exit code. Everything else lives in a function that returns an
// error instead of calling os.Exit, which is what makes it callable from a
// test.
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

// config holds the parsed command line. A struct rather than three loose
// strings so run's signature stays short as later rungs add flags.
type config struct {
	root string
	db   string
	addr string
}

func main() {
	// Signal handling stays in main. run receives the resulting context and
	// doesn't care where the cancellation came from — a real SIGINT in
	// production, a cancel() call in a test. Same code path either way.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, os.Args[1:], os.Stderr); err != nil {
		// --help is not a failure. flag has already printed the usage text,
		// so exit quietly with 0.
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, "gofs:", err)
		os.Exit(1)
	}
}

// run wires everything together and blocks until ctx is cancelled or the
// server fails. It returns an error rather than exiting so that main owns
// the process lifecycle and run stays testable.
func run(ctx context.Context, args []string, stderr io.Writer) error {
	cfg, err := parseFlags(args, stderr)
	if err != nil {
		return err
	}

	log.Printf("root=%s, db=%s, addr=%s", cfg.root, cfg.db, cfg.addr)

	srv := &http.Server{
		Addr:              cfg.addr,
		Handler:           newMux(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		// WriteTimeout is unset on purpose: it caps the time to write an
		// entire response, which would truncate any download slower than
		// the limit. ReadTimeout is omitted for the same reason on uploads.
	}

	// ListenAndServe blocks until the server stops, so it runs on its own
	// goroutine. The buffer of 1 lets that goroutine deliver its result and
	// exit even after we've stopped receiving — which is exactly what
	// happens on the shutdown path below.
	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		// The server died on its own: port in use, permission denied.
		// Nothing to shut down.
		return err
	case <-ctx.Done():
		log.Println("shutting down")
	}

	// A fresh context: ctx is already cancelled, and passing it would make
	// Shutdown give up before it started. This budget is how long in-flight
	// requests get to finish.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	return srv.Shutdown(shutdownCtx)
}

// parseFlags builds a private FlagSet rather than using the package-level
// flag functions. Two reasons: it reads the args passed in instead of the
// global os.Args, and ContinueOnError makes it return an error instead of
// calling os.Exit on bad input. Both are what let a test drive it.
func parseFlags(args []string, stderr io.Writer) (config, error) {
	fs := flag.NewFlagSet("gofs", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var cfg config
	fs.StringVar(&cfg.root, "root", "", "`dir` to serve (required)")
	fs.StringVar(&cfg.db, "db", "./index.db", "SQLite index `file`, kept outside --root")
	fs.StringVar(&cfg.addr, "addr", "127.0.0.1:8080", "`host:port` to listen on (0.0.0.0 exposes to the LAN)")

	if err := fs.Parse(args); err != nil {
		return config{}, err
	}
	if cfg.root == "" {
		return config{}, errors.New("missing required flag: --root")
	}
	return cfg, nil
}

// newMux is the one place routes are registered, so a test asserting on
// routing is asserting on the real table rather than a copy of it.
func newMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/healthz", healthz)
	return mux
}

func healthz(w http.ResponseWriter, r *http.Request) {
	w.Write([]byte("OK"))
}

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"go-file-system/internal/api"
	"go-file-system/internal/disk"
	"go-file-system/internal/index"
	"go-file-system/internal/scan"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

type config struct {
	root string
	db   string
	addr string
}

func main() {

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, os.Args[1:], os.Stderr); err != nil {

		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, "gofs:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stderr io.Writer) error {
	cfg, err := parseFlags(args, stderr)
	if err != nil {
		return err
	}

	log.Printf("root=%s, db=%s, addr=%s", cfg.root, cfg.db, cfg.addr)

	root, err := disk.New(cfg.root)
	if err != nil {
		return err
	}
	defer root.Close()

	idx, err := index.Open(cfg.db)
	if err != nil {
		return err
	}
	defer idx.Close()

	scanner, err := scan.New(root, idx, cfg.db)
	if err != nil {
		return err
	}

	result, err := scanner.Scan(ctx)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	}
	log.Printf("initial scan: scanned=%d removed=%d in %s",
		result.Scanned, result.Removed, result.Duration.Round(time.Millisecond))

	srv := &http.Server{
		Addr:              cfg.addr,
		Handler:           api.New(root, idx, scanner).Routes(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:

		return err
	case <-ctx.Done():
		log.Println("shutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	return srv.Shutdown(shutdownCtx)
}

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

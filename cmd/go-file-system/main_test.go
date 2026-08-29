package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestHealthz calls the handler directly — no server, no port, no network.
// A handler is just a function taking (ResponseWriter, *Request), so a test
// can call it like any other function. httptest.NewRecorder is a
// ResponseWriter that keeps everything in memory instead of writing to a
// socket.
func TestHealthz(t *testing.T) {
	// httptest.NewRequest builds a server-side request. It panics rather
	// than returning an error, which is what you want in a test: a
	// malformed literal URL is a bug in the test, not a case to handle.
	req := httptest.NewRequest(http.MethodGet, "/api/healthz", nil)
	rec := httptest.NewRecorder()

	healthz(rec, req)

	res := rec.Result()
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d", res.StatusCode, http.StatusOK)
	}

	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if got, want := string(body), "OK"; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}

	// Nothing in healthz sets a Content-Type. This asserts the sniffing
	// behaviour: net/http looks at the first bytes written and picks a
	// type. Same mechanism that gives downloads their content type for
	// free at rung 6.
	if got, want := res.Header.Get("Content-Type"), "text/plain; charset=utf-8"; got != want {
		t.Errorf("Content-Type = %q, want %q", got, want)
	}
}

// TestRouting drives newMux — the same function main uses — so deleting a
// route from the real table breaks this test. That is the whole point of
// extracting it.
func TestRouting(t *testing.T) {
	mux := newMux()

	tests := []struct {
		name   string
		method string
		target string
		want   int
	}{
		{"get", http.MethodGet, "/api/healthz", http.StatusOK},
		// A GET pattern also matches HEAD. Free, and useful at rung 6 when
		// a client wants a file's size without downloading it.
		{"head", http.MethodHead, "/api/healthz", http.StatusOK},
		// Path matches but method doesn't: ServeMux answers 405 itself,
		// with an Allow header. Before Go 1.22 you wrote that by hand.
		{"post is rejected", http.MethodPost, "/api/healthz", http.StatusMethodNotAllowed},
		{"unknown path", http.MethodGet, "/api/nope", http.StatusNotFound},
		// Trailing slash is a different pattern. "/api/healthz" is an exact
		// match; only a registered pattern ending in "/" matches a subtree.
		{"trailing slash", http.MethodGet, "/api/healthz/", http.StatusNotFound},
	}

	for _, tt := range tests {
		// t.Run makes each row a named subtest, so a failure reports
		// TestRouting/post_is_rejected rather than a bare line number.
		// Run one with: go test -run 'TestRouting/post'
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.target, nil)
			rec := httptest.NewRecorder()

			mux.ServeHTTP(rec, req)

			if rec.Code != tt.want {
				t.Errorf("%s %s = %d, want %d", tt.method, tt.target, rec.Code, tt.want)
			}
		})
	}

	// The Allow header is part of a correct 405. Note it reads "GET, HEAD"
	// even though only GET was registered — independent confirmation of the
	// HEAD row above.
	t.Run("405 carries Allow", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/healthz", nil)
		rec := httptest.NewRecorder()

		mux.ServeHTTP(rec, req)

		if got, want := rec.Header().Get("Allow"), "GET, HEAD"; got != want {
			t.Errorf("Allow = %q, want %q", got, want)
		}
	})
}

// TestParseFlags is what the refactor bought. The package-level flag
// functions read os.Args and call os.Exit on bad input, so none of this was
// reachable before.
func TestParseFlags(t *testing.T) {
	t.Run("defaults apply when only root is given", func(t *testing.T) {
		cfg, err := parseFlags([]string{"--root", "/tmp"}, io.Discard)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.root != "/tmp" {
			t.Errorf("root = %q, want %q", cfg.root, "/tmp")
		}
		if cfg.db != "./index.db" {
			t.Errorf("db = %q, want %q", cfg.db, "./index.db")
		}
		if cfg.addr != "127.0.0.1:8080" {
			t.Errorf("addr = %q, want %q", cfg.addr, "127.0.0.1:8080")
		}
	})

	t.Run("all flags override", func(t *testing.T) {
		cfg, err := parseFlags(
			[]string{"--root", "/srv", "--db", "/var/i.db", "--addr", "0.0.0.0:9000"},
			io.Discard,
		)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := config{root: "/srv", db: "/var/i.db", addr: "0.0.0.0:9000"}
		if cfg != want {
			t.Errorf("cfg = %+v, want %+v", cfg, want)
		}
	})

	// The rule flag itself cannot express: --root has no default and the
	// program must refuse to start without it.
	t.Run("missing root is an error", func(t *testing.T) {
		if _, err := parseFlags(nil, io.Discard); err == nil {
			t.Error("expected an error when --root is absent")
		}
	})

	t.Run("unknown flag is an error", func(t *testing.T) {
		if _, err := parseFlags([]string{"--root", "/tmp", "--nope"}, io.Discard); err == nil {
			t.Error("expected an error for an unrecognised flag")
		}
	})

	// -h must be distinguishable from a real failure, because main exits 0
	// on it. errors.Is is what makes that check work through wrapping.
	t.Run("help is reported as ErrHelp", func(t *testing.T) {
		_, err := parseFlags([]string{"-h"}, io.Discard)
		if !errors.Is(err, flag.ErrHelp) {
			t.Errorf("err = %v, want flag.ErrHelp", err)
		}
	})
}

// TestRunShutsDown exercises sections 6-9 without sending a signal. run
// takes a context, so cancelling it here follows the identical path a real
// SIGINT takes: select picks ctx.Done, Shutdown drains, run returns nil.
//
// Port 0 asks the kernel for any free port, so this never collides with a
// server you have running.
func TestRunShutsDown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- run(ctx, []string{"--root", t.TempDir(), "--addr", "127.0.0.1:0"}, io.Discard)
	}()

	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("run returned %v, want nil on clean shutdown", err)
		}
	case <-time.After(5 * time.Second):
		// If this fires, shutdown is wedged — the failure the whole
		// select/Shutdown dance exists to prevent.
		t.Fatal("run did not return within 5s of cancellation")
	}
}

// TestRunRejectsBadFlags confirms run surfaces a flag error rather than
// starting a server on nothing.
func TestRunRejectsBadFlags(t *testing.T) {
	if err := run(context.Background(), nil, io.Discard); err == nil {
		t.Error("expected an error when --root is absent")
	}
}

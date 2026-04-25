package main

import (
	"context"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/transport"
	"agent-overflow/internal/wsllauncher"
)

// These tests cover the small Phase D helpers in main.go (flag
// parsing, listen-addr splitting, port extraction). The full
// runHeadless code path isn't unit-testable because it owns the
// process lifecycle (signal.Notify, fatalf-on-failure), but the
// helpers carry the parsing logic that's most likely to break.

func TestPortFromAddr(t *testing.T) {
	cases := []struct {
		addr string
		want int
	}{
		{"127.0.0.1:54321", 54321},
		{"[::1]:8080", 8080},
		{"0.0.0.0:80", 80},
		{"", 0},
		{"not-an-addr", 0},
		{"127.0.0.1:notnum", 0},
	}
	for _, c := range cases {
		if got := portFromAddr(c.addr); got != c.want {
			t.Errorf("portFromAddr(%q) = %d, want %d", c.addr, got, c.want)
		}
	}
}

func TestParseFlagsConnectMode(t *testing.T) {
	cases := []struct {
		name        string
		args        []string
		wantConnect string
		wantHeadles bool
	}{
		{
			name:        "default desktop",
			args:        nil,
			wantConnect: "",
			wantHeadles: false,
		},
		{
			name:        "connect URL",
			args:        []string{"--connect", "ws://host:1234?token=t"},
			wantConnect: "ws://host:1234?token=t",
			wantHeadles: false,
		},
		{
			name:        "headless via fd",
			args:        []string{"--print-url-fd", "3"},
			wantConnect: "",
			wantHeadles: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseFlags(c.args)
			if err != nil {
				t.Fatalf("parseFlags(%v) unexpected error: %v", c.args, err)
			}
			if got.connect != c.wantConnect {
				t.Errorf("connect = %q, want %q", got.connect, c.wantConnect)
			}
			if got.headless != c.wantHeadles {
				t.Errorf("headless = %v, want %v", got.headless, c.wantHeadles)
			}
		})
	}
}

// TestParseFlagsConnectAndListenRejected pins the explicit rejection of
// `--connect` + `--listen`. The flags configure mutually exclusive
// modes — --listen sets the local transport bind, --connect skips the
// local transport entirely — so silently dropping one would be
// confusing. The error message must point at the conflict so the
// operator notices before the desktop window opens.
func TestParseFlagsConnectAndListenRejected(t *testing.T) {
	_, err := parseFlags([]string{"--connect", "ws://host:1234?token=t", "--listen", "127.0.0.1:0"})
	if err == nil {
		t.Fatal("parseFlags accepted --connect + --listen, want error")
	}
	if !strings.Contains(err.Error(), "--connect") || !strings.Contains(err.Error(), "--listen") {
		t.Fatalf("error %q does not mention both flags", err)
	}
}

// TestParseFlagsConnectAndPrintURLFDRejected covers the existing pair
// of conflicting flags through the same surface; without it the
// fatalf-to-error refactor could regress the original rejection.
func TestParseFlagsConnectAndPrintURLFDRejected(t *testing.T) {
	_, err := parseFlags([]string{"--connect", "ws://host:1234?token=t", "--print-url-fd", "3"})
	if err == nil {
		t.Fatal("parseFlags accepted --connect + --print-url-fd, want error")
	}
	if !strings.Contains(err.Error(), "--connect") || !strings.Contains(err.Error(), "--print-url-fd") {
		t.Fatalf("error %q does not mention both flags", err)
	}
}

func TestSplitListenAddr(t *testing.T) {
	cases := []struct {
		in       string
		wantHost string
		wantPort int
	}{
		{"127.0.0.1:0", "127.0.0.1", 0},
		{"0.0.0.0:8080", "0.0.0.0", 8080},
		{":0", "127.0.0.1", 0},
		{"bogus", "127.0.0.1", 0},
		{"host:nan", "host", 0},
	}
	for _, c := range cases {
		gotHost, gotPort := splitListenAddr(c.in)
		if gotHost != c.wantHost || gotPort != c.wantPort {
			t.Errorf("splitListenAddr(%q) = (%q,%d); want (%q,%d)", c.in, gotHost, gotPort, c.wantHost, c.wantPort)
		}
	}
}

func TestBootstrapStdoutPrefixIsStable(t *testing.T) {
	// The Windows-side launcher (cmd/agent-overflow-windows) and the
	// wsllauncher package's DefaultBootstrapPrefix must agree on the
	// sentinel. The launcher matches by string prefix; if either side
	// drifts we silently break the IPC and the WebView never opens.
	const expected = "__AO_BOOTSTRAP__:"
	if bootstrapStdoutPrefix != expected {
		t.Fatalf("bootstrapStdoutPrefix changed (got %q, want %q); update the launcher's DefaultBootstrapPrefix to match",
			bootstrapStdoutPrefix, expected)
	}
	if wsllauncher.DefaultBootstrapPrefix != expected {
		t.Fatalf("wsllauncher.DefaultBootstrapPrefix drifted from main's bootstrapStdoutPrefix (got %q, want %q)",
			wsllauncher.DefaultBootstrapPrefix, expected)
	}
	if !strings.HasPrefix(bootstrapStdoutPrefix, "__") || !strings.HasSuffix(bootstrapStdoutPrefix, ":") {
		t.Fatalf("bootstrap prefix shape changed")
	}
}

// TestBootstrapWireRoundTrip verifies the writeBootstrap (Linux side) →
// readBootstrapLine (Windows-side launcher) handshake survives the
// stdout sentinel fallback path. We boot a real transport.Server so
// the {port, token} writeBootstrap reads are real values, swap
// os.Stdout for an os.Pipe, run writeBootstrap with fd=0 (forcing the
// stdout fallback), then drive the launcher's parser against the read
// half of the pipe.
//
// Fd-3 fallback semantics aren't exercised here — the Windows
// launcher only knows the stdout path, and the fd-3 happy path is
// already covered by the wsllauncher package's unit tests.
func TestBootstrapWireRoundTrip(t *testing.T) {
	dispatcher := transport.NewDispatcher()
	bus := transport.NewEventBus(64)
	srv, err := transport.New(transport.Config{
		Dispatcher: dispatcher,
		EventBus:   bus,
		Token:      "round-trip-token",
		BindAddr:   "127.0.0.1",
	})
	if err != nil {
		t.Fatalf("transport.New: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("transport.Server.Start: %v", err)
	}
	t.Cleanup(func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	})

	// Sanity-check the addr/token before we ship them through the
	// pipe: a 0-port or empty-token would make the launcher reject the
	// frame and obscure the actual round-trip we're testing.
	if srv.Token() == "" {
		t.Fatalf("server returned empty token")
	}
	_, _, splitErr := net.SplitHostPort(srv.Addr())
	if splitErr != nil {
		t.Fatalf("server addr %q not host:port: %v", srv.Addr(), splitErr)
	}

	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	t.Cleanup(func() {
		_ = pr.Close()
		_ = pw.Close()
	})

	// Swap stdout for the pipe write-half. Any t.Logf, log.Printf, etc.
	// during this window would land in the pipe too — which is
	// realistic, the launcher tolerates pre-bootstrap chatter.
	origStdout := os.Stdout
	os.Stdout = pw
	t.Cleanup(func() { os.Stdout = origStdout })

	// fd=0 forces writeBootstrap to skip the fd-3 fast path and emit
	// the sentinel-prefixed line on stdout. (fd=0 is stdin in Unix,
	// not a valid bootstrap target — the function treats anything
	// non-positive as "use stdout".)
	if err := writeBootstrap(0, srv); err != nil {
		t.Fatalf("writeBootstrap: %v", err)
	}
	// Closing the write half lets the scanner observe EOF if the
	// bootstrap line wasn't found — without this, readBootstrapLine
	// would block forever.
	if err := pw.Close(); err != nil {
		t.Fatalf("close pw: %v", err)
	}

	// Drive the wsllauncher parser against the read half. We use the
	// same DefaultBootstrapPrefix the production launcher reaches for.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	bs, err := wsllauncher.ReadBootstrapForTest(ctx, pr, wsllauncher.DefaultBootstrapPrefix)
	if err != nil {
		t.Fatalf("readBootstrapLine: %v", err)
	}

	wantPort := portFromAddr(srv.Addr())
	if bs.Port != wantPort {
		t.Fatalf("round-trip port = %d, want %d", bs.Port, wantPort)
	}
	if bs.Token != srv.Token() {
		t.Fatalf("round-trip token = %q, want %q", bs.Token, srv.Token())
	}
}

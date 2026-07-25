// Package pprofserve exposes net/http/pprof on an opt-in, loopback-only
// diagnostic listener.
//
// It is deliberately a separate listener rather than routes on the
// transport server: profiling endpoints carry process internals and
// must never ride the authenticated wire surface (see
// internal/transport/AGENTS.md for the authz rules that would otherwise
// apply).
//
// Enable by setting AGENT_OVERFLOW_PPROF before the backend starts:
//
//	AGENT_OVERFLOW_PPROF=1              # binds the default 127.0.0.1:6363
//	AGENT_OVERFLOW_PPROF=127.0.0.1:7777 # binds an explicit loopback addr
//
// The Windows launcher and the dev supervisor forward the variable
// across the WSL boundary via WSLENV, so setting it in the shell that
// runs `make dev` (or on the Windows side for the production launcher)
// reaches the WSL backend.
package pprofserve

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"strings"
	"time"

	"agent-overflow/internal/diagenv"
)

// EnvVar names the opt-in environment variable.
const EnvVar = diagenv.Pprof

// DefaultAddr is used when the variable is a bare enable ("1"/"true").
const DefaultAddr = "127.0.0.1:6363"

// StartIfEnabled starts the pprof listener when EnvVar opts in.
// Returns the bound address ("" when disabled) and a stop func that
// closes the listener. Errors are returned, never swallowed: a bad
// value means the operator asked for profiling and isn't getting it,
// so the caller must surface it.
func StartIfEnabled() (addr string, stop func(), err error) {
	raw := strings.TrimSpace(os.Getenv(EnvVar))
	switch {
	case raw == "" || raw == "0" || strings.EqualFold(raw, "false"):
		return "", func() {}, nil
	case raw == "1" || strings.EqualFold(raw, "true"):
		raw = DefaultAddr
	}

	host, _, splitErr := net.SplitHostPort(raw)
	if splitErr != nil {
		return "", nil, fmt.Errorf("pprofserve: %s=%q is not host:port: %w", EnvVar, raw, splitErr)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return "", nil, fmt.Errorf("pprofserve: refusing non-loopback bind %q — profiling data stays on localhost", raw)
	}

	ln, err := net.Listen("tcp", raw)
	if err != nil {
		return "", nil, fmt.Errorf("pprofserve: listen %s: %w", raw, err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)

	srv := &http.Server{
		Handler: mux,
		// No Read/WriteTimeout: /debug/pprof/profile?seconds=N and
		// /debug/pprof/trace stream for their full duration by design.
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		if serveErr := srv.Serve(ln); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			// The listener died out from under a running profiler
			// session; say so rather than going silently dark.
			fmt.Fprintf(os.Stderr, "pprofserve: serve: %v\n", serveErr)
		}
	}()
	return ln.Addr().String(), func() { _ = srv.Close() }, nil
}

package codex

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/procutil/procutiltest"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/providertest"
	"agent-overflow/internal/testutil/mockexec"
)

// admissionFake is a fake app-server that answers initialize with
// initializeResult and appends every method it receives to the returned
// log, so a test sees whether anything followed a refused handshake.
func admissionFake(t *testing.T, initializeResult string) (binary, methodLog string) {
	t.Helper()
	dir := t.TempDir()
	methodLog = dir + "/methods"
	script := fmt.Sprintf(`#!/bin/bash
while IFS= read -r line; do
    method=$(echo "$line" | grep -o '"method":"[^"]*"' | head -1)
    echo "$method" >> %q
    id=$(echo "$line" | grep -o '"id":[0-9]*' | head -1 | grep -o '[0-9]*')
    if [ -z "$id" ]; then
        continue
    fi
    if echo "$line" | grep -q '"method":"initialize"'; then
        echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":%s}"
        continue
    fi
    echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{}}"
done
`, methodLog, bashJSON(initializeResult))
	binary = dir + "/codex app-server tools/codex"
	if err := os.MkdirAll(dir+"/codex app-server tools", 0o755); err != nil {
		t.Fatal(err)
	}
	mockexec.Write(t, binary, script)
	return binary, methodLog
}

func loggedMethods(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read method log: %v", err)
	}
	var methods []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line != "" {
			methods = append(methods, strings.TrimSuffix(strings.TrimPrefix(line, `"method":"`), `"`))
		}
	}
	return methods
}

// admissionCases are initialize replies by the build a stock Codex
// reports, answered with the client's own name (initializeResult), or a
// reply it does not read, which is admitted with no build as main admits it.
var admissionCases = []struct {
	name, build, unread, want string
}{
	{
		name:  "last release before the minimum",
		build: "0.153.4",
		want:  "Codex v0.153.4 (%s) is installed; Agent Overflow needs v0.154.0 or newer. Update Codex.",
	},
	{
		name:  "pre-release of the minimum",
		build: "0.154.0-alpha.1",
		want:  "Codex v0.154.0-alpha.1 (%s) is installed; Agent Overflow needs v0.154.0 or newer. Update Codex.",
	},
	{name: "the minimum", build: "0.154.0"},
	{name: "no user agent", unread: `{}`},
	{name: "a user agent that names no build after the originator", unread: `{"userAgent":"codex_cli_rs/dev (Linux)"}`},
}

// initializeResult is the case's reply to a client named clientName.
func initializeResult(build, unread, clientName string) string {
	if build == "" {
		return unread
	}
	return `{"userAgent":"` + providertest.FormatCodexUserAgent(build, clientName, clientName, provider.CodexClientVersion) + `"}`
}

// TestDialAdmitsByInitializeVersion pins the gate every session start, resume
// and send reaches: a shared or dedicated app-server that reports a build
// below the minimum is refused right after initialize with the message the
// user sees, naming the binary that ran, and nothing more is sent to it.
func TestDialAdmitsByInitializeVersion(t *testing.T) {
	t.Parallel()
	for _, tc := range admissionCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			binary, methodLog := admissionFake(t, initializeResult(tc.build, tc.unread, provider.CodexClientOrigin))
			conn, err := Dial(context.Background(), ConnConfig{Binary: binary, WorkDir: t.TempDir(), Reaper: procutiltest.Reaper()})
			if tc.want == "" {
				if err != nil {
					t.Fatalf("Dial: %v", err)
				}
				if got := conn.AppServerVersion(); got != tc.build {
					t.Fatalf("recorded build = %q, want %q", got, tc.build)
				}
				if err := conn.Close(); err != nil {
					t.Fatalf("Close: %v", err)
				}
				return
			}
			assertVersionRefusal(t, err, fmt.Sprintf(tc.want, binary))
			if got := loggedMethods(t, methodLog); len(got) != 1 || got[0] != "initialize" {
				t.Fatalf("methods after a refused handshake = %q, want only initialize", got)
			}
		})
	}
}

// TestOneshotClientAdmitsByInitializeVersion pins the same gate on the
// one-shot app-server behind fork, transfer discovery, model and skill
// listing and MCP sign-in.
func TestOneshotClientAdmitsByInitializeVersion(t *testing.T) {
	t.Parallel()
	for _, tc := range admissionCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			binary, methodLog := admissionFake(t, initializeResult(tc.build, tc.unread, "admission-test"))
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			client, err := startOneshotClient(ctx, provider.Bounded(procutiltest.Reaper()), oneshotSpec{
				Binary: binary, WorkDir: t.TempDir(), ClientName: "admission-test",
			})
			if tc.want == "" {
				if err != nil {
					t.Fatalf("startOneshotClient: %v", err)
				}
				client.close()
				return
			}
			assertVersionRefusal(t, err, fmt.Sprintf(tc.want, binary))
			if got := loggedMethods(t, methodLog); len(got) != 1 || got[0] != "initialize" {
				t.Fatalf("methods after a refused handshake = %q, want only initialize", got)
			}
		})
	}
}

// assertVersionRefusal checks the refusal's type, its text, and that a
// caller's wrapping does not reach the RPC answer.
func assertVersionRefusal(t *testing.T, err error, want string) {
	t.Helper()
	var versionErr *provider.CodexVersionError
	if !errors.As(err, &versionErr) || !errors.Is(err, provider.ErrCodexVersionUnsupported) {
		t.Fatalf("err = %v, want a *provider.CodexVersionError", err)
	}
	if err.Error() != want {
		t.Fatalf("err = %q, want %q", err.Error(), want)
	}
	code, message, ok := errorsx.PublicDetails(fmt.Errorf("send message: start session: %w", err))
	if !ok || code != provider.CodexVersionUnsupportedCode || message != want {
		t.Fatalf("public details = %q %q %v, want %q", code, message, ok, want)
	}
}

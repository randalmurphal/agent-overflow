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

var admissionCases = []struct {
	name, initializeResult, want string
}{
	{
		name:             "last release before the minimum",
		initializeResult: `{"userAgent":"codex_cli_rs/0.153.4 (Ubuntu 24.04; x86_64) codex_cli_rs/0.153.4"}`,
		want:             "Codex v0.153.4 (%s) is installed; Agent Overflow needs v0.154.0 or newer. Update Codex.",
	},
	{
		name:             "pre-release of the minimum",
		initializeResult: `{"userAgent":"codex_cli_rs/0.154.0-alpha.1 (Linux)"}`,
		want:             "Codex v0.154.0-alpha.1 (%s) is installed; Agent Overflow needs v0.154.0 or newer. Update Codex.",
	},
	{
		name:             "no user agent",
		initializeResult: `{}`,
		want:             "Agent Overflow could not read the version of Codex (%s). Update Codex to v0.154.0 or newer.",
	},
	{
		name:             "unreadable user agent",
		initializeResult: `{"userAgent":"codex_cli_rs/dev (Linux)"}`,
		want:             `Agent Overflow could not read the version of Codex (%s) from "codex_cli_rs/dev (Linux)". Update Codex to v0.154.0 or newer.`,
	},
	{
		name:             "the minimum",
		initializeResult: `{"userAgent":"codex_cli_rs/0.154.0 (Linux)"}`,
	},
}

// TestDialAdmitsByInitializeVersion pins the gate every session start, resume
// and send reaches: a shared or dedicated app-server below the minimum, or
// one whose version cannot be read, is refused right after initialize with
// the message the user sees, naming the binary that ran, and nothing more is
// sent to it.
func TestDialAdmitsByInitializeVersion(t *testing.T) {
	t.Parallel()
	for _, tc := range admissionCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			binary, methodLog := admissionFake(t, tc.initializeResult)
			conn, err := Dial(context.Background(), ConnConfig{Binary: binary, WorkDir: t.TempDir(), Reaper: procutiltest.Reaper()})
			if tc.want == "" {
				if err != nil {
					t.Fatalf("Dial: %v", err)
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
			binary, methodLog := admissionFake(t, tc.initializeResult)
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

package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"agent-overflow/internal/provider"
)

// The app-server's build version, learned from the `initialize` handshake.
//
// `initialize` carries no capability list and no protocol version a client
// can branch on; the only in-band statement of the running build is
// `InitializeResponse.userAgent`, formatted upstream as
// `{originator}/{build} ({os} {os version}; {arch}) {terminal}`
// (codex-rs/login/src/auth/default_client.rs get_codex_user_agent), e.g.
// `codex_cli_rs/0.154.0 (Ubuntu 24.04; x86_64) …`. Every client that
// completes a handshake admits the process by it through admitInitialize,
// before its first other request. It describes the process actually on the
// other end of the pipe, not whatever binary the setting named when the app
// booted.

// AppServerVersion returns the codex build this session's process reported at
// `initialize`, or "" when it could not be determined.
func (s *Session) AppServerVersion() string {
	if s.conn == nil {
		return ""
	}
	return s.conn.AppServerVersion()
}

// admitInitialize is the one check of an app-server's `initialize` reply.
// path is the executable that answered and result the reply. It returns the
// build the reply reports, or the *provider.CodexVersionError refusing the
// process, unwrapped so every client refuses with the same message.
func admitInitialize(path string, result json.RawMessage) (string, error) {
	userAgent := readNestedString(result, "userAgent")
	version := parseAppServerVersion(userAgent)
	if err := provider.CheckCodexVersion(path, version, userAgent); err != nil {
		return "", err
	}
	return version, nil
}

// parseAppServerVersion pulls the build version out of a codex user agent.
// The product prefix ends where the OS block ` (` starts; the build is the
// component after the prefix's last `/`, because an originator override may
// itself contain slashes (`team/2.0/0.154.0 (…)`). The whole component must
// be a build version: `dev-0.154.0` is unreadable, not 0.154.0. The OS
// block and anything after it carry digits of their own and are never read.
//
// Anything without that shape yields "", never a guess.
func parseAppServerVersion(userAgent string) string {
	prefix, _, found := strings.Cut(userAgent, " (")
	if !found {
		return ""
	}
	slash := strings.LastIndexByte(prefix, '/')
	if slash < 0 {
		return ""
	}
	return provider.ParseCodexBuildVersion(prefix[slash+1:])
}

// lineHandshake runs `initialize` for a client that reads frames from lines
// (startLineReader): it writes the request with id, waits for that reply,
// admits it and writes `initialized`. Frames before the reply are skipped.
func lineHandshake(
	ctx context.Context,
	proc *provider.Process,
	lines <-chan readResult,
	id int64,
	params any,
) error {
	if err := writeJSONRPC(proc, map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  "initialize",
		"params":  params,
	}); err != nil {
		return fmt.Errorf("write initialize: %w", err)
	}
	result, err := awaitLineResponse(ctx, lines, id, "initialize")
	if err != nil {
		return err
	}
	if _, err := admitInitialize(proc.Path(), result); err != nil {
		return err
	}
	if err := writeJSONRPC(proc, map[string]any{
		"jsonrpc": "2.0",
		"method":  "initialized",
	}); err != nil {
		return fmt.Errorf("write initialized: %w", err)
	}
	return nil
}

// awaitLineResponse waits on lines for the response to request id, skipping
// every other frame, and returns its result or its JSON-RPC error.
func awaitLineResponse(ctx context.Context, lines <-chan readResult, id int64, method string) (json.RawMessage, error) {
	for {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("await %s: %w", method, ctx.Err())
		case r := <-lines:
			if r.err != nil {
				if errors.Is(r.err, io.EOF) {
					return nil, fmt.Errorf("app-server exited before answering %s", method)
				}
				return nil, fmt.Errorf("read %s: %w", method, r.err)
			}
			var envelope struct {
				ID     *json.Number    `json:"id,omitempty"`
				Result json.RawMessage `json:"result,omitempty"`
				Error  *rpcError       `json:"error,omitempty"`
			}
			dec := json.NewDecoder(bytes.NewReader(r.line))
			dec.UseNumber()
			if err := dec.Decode(&envelope); err != nil || envelope.ID == nil {
				continue
			}
			if got, err := envelope.ID.Int64(); err != nil || got != id {
				continue
			}
			if envelope.Error != nil {
				return nil, fmt.Errorf("%s: %s (code %d)", method, envelope.Error.Message, envelope.Error.Code)
			}
			return envelope.Result, nil
		}
	}
}

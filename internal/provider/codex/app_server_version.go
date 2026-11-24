package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"unicode/utf8"

	"agent-overflow/internal/provider"
)

// The app-server's build version, learned from the `initialize` handshake.
//
// `initialize` carries no capability list and no protocol version a client
// can branch on; the only in-band statement of the running build is
// `InitializeResponse.userAgent`. Upstream assembles it as
// `{originator}/{build} ({os type} {os version}; {arch}) {terminal}`, then
// ` ({client name}; {client version})` (rust-v0.154.0
// codex-rs/login/src/auth/default_client.rs get_codex_user_agent). Every
// field is written unescaped, so a launcher that changes the originator can
// make any reply text, including one byte for byte equal to another
// launch's. The reader therefore reads only what it can attribute: the
// originator this side launched the process with (CODEX_INTERNAL_ORIGINATOR_OVERRIDE
// from its environment when that is set, otherwise the clientInfo.name the
// handshake sent; codex-rs/app-server/src/request_processors/initialize_processor.rs),
// then `/`, a whole build and ` (`. That build is checked against the
// minimum. Any other reply is not read: the process is admitted, as a
// `--version` output with no version in it is (provider.DetectProvider),
// with no recorded build. Every client that completes a handshake admits
// the process through admitInitialize, before its first other request.

// AppServerVersion returns the codex build this session's process reported at
// `initialize`, or "" when it could not be determined.
func (s *Session) AppServerVersion() string {
	if s.conn == nil {
		return ""
	}
	return s.conn.AppServerVersion()
}

// codexOriginatorOverrideEnv names the variable that replaces a Codex
// process's originator, and codexDefaultOriginator is what Codex uses when
// that variable holds a value no HTTP header can carry.
const (
	codexOriginatorOverrideEnv = "CODEX_INTERNAL_ORIGINATOR_OVERRIDE"
	codexDefaultOriginator     = "codex_cli_rs"
)

// handshakeOriginator is the originator a fresh app-server starts its user
// agent with: the override in the environment it was launched with, when
// set, otherwise the client name params sends. lookup reads that
// environment. An override that is not UTF-8 is unset to Codex, and one
// that is not a valid header value is replaced by the default
// (default_client.rs get_originator_value).
func handshakeOriginator(params map[string]any, lookup func(string) (string, bool)) string {
	if override, ok := lookup(codexOriginatorOverrideEnv); ok && utf8.ValidString(override) {
		if validHeaderValue(override) {
			return override
		}
		return codexDefaultOriginator
	}
	info, _ := params["clientInfo"].(map[string]any)
	name, _ := info["name"].(string)
	return name
}

// validHeaderValue is the check Codex's HeaderValue::from_str makes: tab,
// or a byte from space up, except DEL.
func validHeaderValue(value string) bool {
	for i := 0; i < len(value); i++ {
		if b := value[i]; b != '\t' && (b < ' ' || b == 0x7f) {
			return false
		}
	}
	return true
}

// admitInitialize is the one check of an app-server's `initialize` reply.
// path is the executable that answered, originator what its user agent
// starts with (handshakeOriginator) and result the reply. It returns the
// build the reply reports, an owned copy, or the *provider.CodexVersionError
// refusing a build below the minimum, unwrapped so every client refuses with
// the same message. A reply it cannot read is admitted with no build and
// logged once, here, for the one handshake of the process.
func admitInitialize(path, originator string, result json.RawMessage) (string, error) {
	userAgent := readNestedString(result, "userAgent")
	version := appServerBuild(userAgent, originator)
	if version == "" {
		log.Printf("codex: %s answered initialize with a user agent that does not report its build after %q; admitted without a version check: %q",
			path, originator, userAgentForLog(userAgent))
		return "", nil
	}
	if err := provider.CheckCodexVersion(path, version); err != nil {
		return "", err
	}
	return version, nil
}

// appServerBuild is the build userAgent reports right after originator
// and its slash: a whole version (provider.ParseCodexVersion) followed by
// the OS block's " (". "" for any other reply.
func appServerBuild(userAgent, originator string) string {
	rest, ok := strings.CutPrefix(userAgent, originator+"/")
	if !ok {
		return ""
	}
	word, after, ok := strings.Cut(rest, " ")
	if !ok || !strings.HasPrefix(after, "(") {
		return ""
	}
	version, _ := provider.ParseCodexVersion(word)
	return version
}

// userAgentLogLimit bounds the provider text a log line quotes.
const userAgentLogLimit = 160

// userAgentForLog is userAgent as a log line quotes it: provider text, so
// capped, and %q escapes what it does not print.
func userAgentForLog(userAgent string) string {
	if len(userAgent) <= userAgentLogLimit {
		return userAgent
	}
	cut := userAgentLogLimit
	for cut > 0 && !utf8.RuneStart(userAgent[cut]) {
		cut--
	}
	return userAgent[:cut] + "..."
}

// lineHandshake runs `initialize` for a client that reads frames from lines
// (startLineReader): it writes the request with id, waits for that reply,
// admits it and writes `initialized`. Frames before the reply are skipped.
func lineHandshake(
	ctx context.Context,
	proc *provider.Process,
	lines <-chan readResult,
	id int64,
	params map[string]any,
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
	if _, err := admitInitialize(proc.Path(), handshakeOriginator(params, proc.LookupEnv), result); err != nil {
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

package aocli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"agent-overflow/internal/transport"
)

// The AO_* environment contract (spec §5). Agent Overflow injects these into
// every provider session it starts; an `agent-overflow` process inherits them
// and needs no configuration, no discovery, and no credential of its own. The
// names are
// declared here rather than at the injection site so the writer (the app's
// session env assembly) and the reader (this package) cannot drift apart.
//
//   - AO_ENDPOINT — the running app's loopback base URL, no token, no path.
//   - AO_TOKEN    — a scoped credential valid for exactly this session's
//     lifetime and no wider than this session's authority.
//   - AO_THREAD_ID — the thread the session belongs to.
//   - AO_RUN_ID / AO_PHASE_ID — set for workflow phase and unit sessions only;
//     they say which run and phase the caller is part of. An interactive chat
//     session has neither.
const (
	EnvEndpoint = "AO_ENDPOINT"
	EnvToken    = "AO_TOKEN"
	EnvThreadID = "AO_THREAD_ID"
	EnvRunID    = "AO_RUN_ID"
	EnvPhaseID  = "AO_PHASE_ID"
)

// Session is the ambient credential an `agent-overflow` process inherits.
type Session struct {
	Endpoint string
	Token    string
	ThreadID string
	RunID    string
	PhaseID  string
}

// InsidePhase reports whether this session is a workflow phase or unit rather
// than a conversation.
func (s Session) InsidePhase() bool { return s.RunID != "" && s.PhaseID != "" }

// errNoSession is what every execution command reports when it was run outside
// an Agent Overflow session. It is a distinct sentinel because "you are not in
// a session" and "the app refused you" are different problems for whoever reads
// the output, and only the first one is fixed by running the command elsewhere.
var errNoSession = errors.New(
	"not inside an Agent Overflow session: " + EnvEndpoint + " and " + EnvToken +
		" are unset. These commands run from an agent session Agent Overflow started; " +
		"`agent-overflow workflow` commands work anywhere")

// SessionFromEnv reads the contract above. lookup is injected so tests exercise
// the parsing without mutating process environment.
func SessionFromEnv(lookup func(string) (string, bool)) (Session, error) {
	if lookup == nil {
		lookup = os.LookupEnv
	}
	read := func(name string) string {
		value, _ := lookup(name)
		return strings.TrimSpace(value)
	}
	session := Session{
		Endpoint: read(EnvEndpoint),
		Token:    read(EnvToken),
		ThreadID: read(EnvThreadID),
		RunID:    read(EnvRunID),
		PhaseID:  read(EnvPhaseID),
	}
	if session.Endpoint == "" && session.Token == "" {
		return Session{}, errNoSession
	}
	// One of the two present is a broken injection, not an absent one. Saying so
	// beats an unauthorized response the caller would read as a permission
	// problem.
	if session.Endpoint == "" {
		return Session{}, fmt.Errorf("%s is set but %s is not; the session environment is incomplete", EnvToken, EnvEndpoint)
	}
	if session.Token == "" {
		return Session{}, fmt.Errorf("%s is set but %s is not; the session environment is incomplete", EnvEndpoint, EnvToken)
	}
	return session, nil
}

// rpcTimeout bounds one call. Every method behind the scoped surface is a
// SQLite read or a run start that detaches its provisioning, so a call that
// takes this long is a wedged backend rather than slow work.
const rpcTimeout = 30 * time.Second

// client speaks the scoped HTTP RPC route. It is deliberately built on the
// transport package's own frame types: the CLI and the server must agree about
// the wire, and a duplicated struct here would be free to drift.
type client struct {
	session Session
	http    *http.Client
}

func newClient(session Session) *client {
	return &client{session: session, http: &http.Client{Timeout: rpcTimeout}}
}

// rpcError is a refusal the backend expressed in the frame envelope, keeping
// the machine-readable code available to callers that need to distinguish an
// authorization refusal from a method failure.
type rpcError struct {
	Code    string
	Message string
}

func (e *rpcError) Error() string {
	if e.Message == "" {
		return e.Code
	}
	return e.Message
}

// call invokes one method by name and returns its raw JSON result. Raw, because
// `--json` prints the app's own result shape verbatim: the CLI must not become a
// second definition of what a run status looks like. Human rendering decodes
// only the fields it prints, into narrow local structs.
func (c *client) call(method string, params ...any) (json.RawMessage, error) {
	encoded := make([]json.RawMessage, 0, len(params))
	for i, param := range params {
		raw, err := json.Marshal(param)
		if err != nil {
			return nil, fmt.Errorf("encode %s parameter %d: %w", method, i, err)
		}
		encoded = append(encoded, raw)
	}
	body, err := json.Marshal(transport.ClientFrame{Type: "rpc", ID: "1", Method: method, Params: encoded})
	if err != nil {
		return nil, fmt.Errorf("encode %s request: %w", method, err)
	}
	endpoint := strings.TrimSuffix(c.session.Endpoint, "/") + transport.ScopedRPCPath
	request, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build %s request: %w", method, err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+c.session.Token)

	response, err := c.http.Do(request)
	if err != nil {
		return nil, fmt.Errorf("call %s: %w", method, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, httpStatusError(method, response)
	}
	var frame transport.ServerFrame
	if err := json.NewDecoder(response.Body).Decode(&frame); err != nil {
		return nil, fmt.Errorf("decode %s response: %w", method, err)
	}
	if frame.Error != nil {
		return nil, &rpcError{Code: frame.Error.Code, Message: frame.Error.Message}
	}
	return frame.Result, nil
}

// callInto is call plus a decode, for the commands that need typed fields.
func (c *client) callInto(out any, method string, params ...any) (json.RawMessage, error) {
	result, err := c.call(method, params...)
	if err != nil {
		return nil, err
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("call %s: the app returned no result", method)
	}
	if err := json.Unmarshal(result, out); err != nil {
		return nil, fmt.Errorf("decode %s result: %w", method, err)
	}
	return result, nil
}

// httpStatusError translates the transport-level outcomes the RPC envelope
// cannot carry. 401 is the one worth naming: it means the credential has been
// revoked, which for a scoped token means the session that owned it ended.
func httpStatusError(method string, response *http.Response) error {
	if response.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("call %s: this session's %s is no longer valid; the session it belonged to has ended", method, EnvToken)
	}
	detail, _ := io.ReadAll(io.LimitReader(response.Body, 2048))
	message := strings.TrimSpace(string(detail))
	if message == "" {
		message = response.Status
	}
	return fmt.Errorf("call %s: %s", method, message)
}

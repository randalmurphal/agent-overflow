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
	"sync"
	"time"

	"agent-overflow/internal/provider"
)

// Codex sign-in over the app-server. Two variants of one RPC:
// `account/login/start {type:"chatgpt"}` hands back a browser URL this host
// opens, and `{type:"chatgptDeviceCode"}` hands back a constant verification
// page plus a short code the person types on whatever screen they have. Both
// complete on the same `account/login/completed` notification.
//
// Wire shapes, the omitted `jsonrpc` field, and the cancellation answers are
// documented in docs/references/codex-wire.md § Account sign-in.

const (
	// DefaultLoginTimeout is the whole flow's budget, matching the Claude
	// side so one coordinator deadline covers both providers.
	DefaultLoginTimeout = 10 * time.Minute

	// DeviceCodeLifetime is how long a device code is good for. The wire
	// carries NO expiry field, so this is the only place a countdown can come
	// from: it is upstream's own `max_wait` for the device-code poll, and a
	// user watching a timer that outlives the code is worse than no timer.
	DeviceCodeLifetime = 15 * time.Minute
)

// LoginConfig configures one sign-in app-server process.
type LoginConfig struct {
	Binary  string
	WorkDir string
	// Env is the environment this invocation runs with, the same map an
	// account probe takes, and it must carry CODEX_HOME pointed at the
	// isolated login home. Required for the same reason Claude's config dir
	// is: a sign-in must not land in the canonical home before the adoption
	// epilogue has decided where it goes. The rest of the map matters for the
	// reason it matters to a probe — the environment picks which backend
	// answers, and the account adopted must be the one authenticated.
	Env map[string]string
}

// LoginStart is what one account/login/start answered. Which fields are
// filled says which variant ran; the caller does not have to remember.
type LoginStart struct {
	// LoginID correlates the completion notification. It is not optional
	// bookkeeping: a superseded or cancelled login emits a FAILED completion
	// for its own id, so a client that ignored this would report the previous
	// login's failure as this one's.
	LoginID string
	// AuthURL is the browser variant's one-shot authorize URL, carrying the
	// exchange's state. Empty on the device-code variant.
	AuthURL string
	// VerificationURL is the device-code variant's page. It is CONSTANT and
	// carries no code, so it is only useful shown beside UserCode.
	VerificationURL string
	// UserCode is what the person types on the verification page.
	UserCode string
	// ExpiresAt bounds the device-code variant. Zero on the browser variant,
	// whose URL the provider expires on its own terms.
	ExpiresAt time.Time
}

// LoginSession is one held-open app-server driving one sign-in. Upstream
// allows a single login per process, so the caller owns one of these per
// attempt and closes it when the attempt ends, whichever way it ended.
type LoginSession struct {
	proc *provider.Process

	mu      sync.Mutex
	pending map[int64]chan rpcOutcome
	// waiters is keyed by loginId, because the completion notification is
	// addressed by that and by nothing else.
	waiters map[string]chan loginOutcome
	// settled holds a completion that arrived before anyone waited for it.
	// The read loop can dispatch the notification between account/login/start
	// returning on the wire and the caller reaching WaitForCompletion, and
	// dropping it there would hang the flow until its deadline.
	settled map[string]loginOutcome
	nextID  int64
	failed  error

	closeOnce sync.Once
}

// maxSettledLogins bounds the early-completion buffer. Upstream serves one
// login per process, so anything past a couple of entries is a server this
// build does not understand rather than a state to hold open for.
const maxSettledLogins = 8

type rpcOutcome struct {
	result json.RawMessage
	err    error
}

type loginOutcome struct {
	success bool
	message string
	err     error
}

func (o loginOutcome) result() error {
	if o.err != nil {
		return o.err
	}
	if o.success {
		return nil
	}
	if text := strings.Join(strings.Fields(o.message), " "); text != "" {
		return fmt.Errorf("codex: sign-in did not complete: %s", text)
	}
	return errors.New("codex: sign-in did not complete")
}

// StartLogin spawns the app-server and completes its handshake. Nothing has
// been authenticated when it returns; call StartBrowserLogin or
// StartDeviceLogin next. The caller owns the returned session and MUST Close
// it.
func StartLogin(ctx context.Context, cfg LoginConfig) (*LoginSession, error) {
	binary := strings.TrimSpace(cfg.Binary)
	if binary == "" {
		binary = "codex"
	}
	if strings.TrimSpace(cfg.Env["CODEX_HOME"]) == "" {
		return nil, errors.New("codex: sign-in requires an isolated CODEX_HOME")
	}
	proc, err := provider.Spawn(ctx, provider.SpawnConfig{
		Binary:   binary,
		Args:     codexAppServerArgs(),
		Dir:      cfg.WorkDir,
		Env:      cfg.Env,
		UnsetEnv: []string{"CODEX_HOME"},
		Provider: "codex",
	})
	if err != nil {
		return nil, fmt.Errorf("codex: sign-in spawn: %w", err)
	}
	session := &LoginSession{
		proc:    proc,
		pending: make(map[int64]chan rpcOutcome),
		waiters: make(map[string]chan loginOutcome),
		settled: make(map[string]loginOutcome),
	}
	go session.readLoop()

	if _, err := session.call(ctx, "initialize", codexInitializeParams(
		"agent_overflow_login",
		// account/login/completed is the ONE notification this client waits
		// on. account/updated follows a success carrying {authMode, planType}
		// and is deliberately opted out: the adoption epilogue probes the
		// login home for identity, which is authoritative and covers the
		// cases this notification cannot describe.
		oneShotOptOutNotificationMethods("account/login/completed"),
	)); err != nil {
		_ = session.Close()
		return nil, err
	}
	if err := session.notify("initialized", nil); err != nil {
		_ = session.Close()
		return nil, err
	}
	return session, nil
}

// StartBrowserLogin runs the variant whose authorize URL this host opens.
func (s *LoginSession) StartBrowserLogin(ctx context.Context) (LoginStart, error) {
	raw, err := s.call(ctx, "account/login/start", map[string]any{
		"type":                  "chatgpt",
		"codexStreamlinedLogin": false,
	})
	if err != nil {
		return LoginStart{}, err
	}
	var result struct {
		Type    string `json:"type"`
		LoginID string `json:"loginId"`
		AuthURL string `json:"authUrl"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return LoginStart{}, fmt.Errorf("codex: decode sign-in start: %w", err)
	}
	if result.Type != "chatgpt" || result.LoginID == "" || result.AuthURL == "" {
		return LoginStart{}, errors.New("codex: sign-in start returned an incomplete browser flow")
	}
	return LoginStart{LoginID: result.LoginID, AuthURL: result.AuthURL}, nil
}

// StartDeviceLogin runs the variant for a person who is not at this machine:
// a constant verification page and a code they type there.
//
// The type string's casing is exact. Upstream matches the discriminant
// literally and answers a wrong spelling by listing every variant it knows,
// which reads as a protocol failure rather than as a typo.
func (s *LoginSession) StartDeviceLogin(ctx context.Context) (LoginStart, error) {
	raw, err := s.call(ctx, "account/login/start", map[string]any{
		"type": "chatgptDeviceCode",
	})
	if err != nil {
		return LoginStart{}, err
	}
	var result struct {
		Type            string `json:"type"`
		LoginID         string `json:"loginId"`
		VerificationURL string `json:"verificationUrl"`
		UserCode        string `json:"userCode"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return LoginStart{}, fmt.Errorf("codex: decode device sign-in start: %w", err)
	}
	if result.Type != "chatgptDeviceCode" || result.LoginID == "" ||
		result.VerificationURL == "" || result.UserCode == "" {
		return LoginStart{}, errors.New("codex: sign-in start returned an incomplete device flow")
	}
	return LoginStart{
		LoginID:         result.LoginID,
		VerificationURL: result.VerificationURL,
		UserCode:        result.UserCode,
		ExpiresAt:       time.Now().Add(DeviceCodeLifetime),
	}, nil
}

// WaitForCompletion blocks until the app-server reports THIS login settled.
// Both variants complete the same way.
func (s *LoginSession) WaitForCompletion(ctx context.Context, loginID string) error {
	if strings.TrimSpace(loginID) == "" {
		return errors.New("codex: waiting for a sign-in requires its login id")
	}
	s.mu.Lock()
	if s.failed != nil {
		err := s.failed
		s.mu.Unlock()
		return err
	}
	if outcome, ok := s.settled[loginID]; ok {
		delete(s.settled, loginID)
		s.mu.Unlock()
		return outcome.result()
	}
	answers := make(chan loginOutcome, 1)
	s.waiters[loginID] = answers
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.waiters, loginID)
		s.mu.Unlock()
	}()

	select {
	case <-ctx.Done():
		return fmt.Errorf("codex: sign-in: %w", ctx.Err())
	case outcome := <-answers:
		return outcome.result()
	}
}

// Cancel asks the app-server to abandon a login. `notFound` is a success:
// after our own cancel, or after the login already settled, there is nothing
// left to cancel and that is the outcome we wanted. Only a malformed id is an
// error, and that is a bug in this client rather than a state.
//
// Cancelling makes upstream emit a FAILED completion for this login id, which
// is exactly why WaitForCompletion correlates: the frame is real, it is just
// not news.
func (s *LoginSession) Cancel(ctx context.Context, loginID string) error {
	if strings.TrimSpace(loginID) == "" {
		return nil
	}
	raw, err := s.call(ctx, "account/login/cancel", map[string]any{"loginId": loginID})
	if err != nil {
		return err
	}
	var result struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil
	}
	if result.Status != "canceled" && result.Status != "notFound" {
		log.Printf("codex: account/login/cancel answered unknown status %q", result.Status)
	}
	return nil
}

// Close ends the process. It is what actually guarantees a cancelled sign-in
// stops: the device-code poll and the browser flow's listener both live in
// this child, and the login home goes with it.
func (s *LoginSession) Close() error {
	var err error
	s.closeOnce.Do(func() { err = s.proc.Close() })
	return err
}

func (s *LoginSession) call(
	ctx context.Context,
	method string,
	params any,
) (json.RawMessage, error) {
	answers := make(chan rpcOutcome, 1)
	s.mu.Lock()
	if s.failed != nil {
		err := s.failed
		s.mu.Unlock()
		return nil, err
	}
	s.nextID++
	id := s.nextID
	s.pending[id] = answers
	s.mu.Unlock()

	request := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		request["params"] = params
	}
	if err := writeJSONRPC(s.proc, request); err != nil {
		s.forget(id)
		return nil, fmt.Errorf("codex: %s: %w", method, err)
	}

	select {
	case <-ctx.Done():
		s.forget(id)
		return nil, fmt.Errorf("codex: %s: %w", method, ctx.Err())
	case outcome := <-answers:
		return outcome.result, outcome.err
	}
}

func (s *LoginSession) notify(method string, params any) error {
	request := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		request["params"] = params
	}
	if err := writeJSONRPC(s.proc, request); err != nil {
		return fmt.Errorf("codex: %s: %w", method, err)
	}
	return nil
}

func (s *LoginSession) forget(id int64) {
	s.mu.Lock()
	delete(s.pending, id)
	s.mu.Unlock()
}

// readLoop owns every read for the process's life. It must outlive any single
// request: a cancel is written while a completion wait is outstanding, and a
// read-per-call arrangement would have the two goroutines racing for lines
// meant for each other.
func (s *LoginSession) readLoop() {
	for {
		line, err := s.proc.ReadLine()
		if err != nil {
			s.failAll(err)
			return
		}
		s.dispatch(line)
	}
}

func (s *LoginSession) dispatch(line []byte) {
	// Responses OMIT the `jsonrpc` field on this app-server, so nothing here
	// may require it. Decoding leniently is the contract, not a tolerance.
	var envelope struct {
		ID     *json.Number    `json:"id"`
		Method string          `json:"method"`
		Result json.RawMessage `json:"result"`
		Params json.RawMessage `json:"params"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.UseNumber()
	if err := decoder.Decode(&envelope); err != nil {
		return
	}
	if envelope.Method != "" {
		if envelope.Method == "account/login/completed" {
			s.settle(envelope.Params)
		}
		// Everything else was opted out at initialize and should not be here.
		// Dropping it is deliberate: a Codex release adding a notification
		// must not break a live sign-in.
		return
	}
	if envelope.ID == nil {
		return
	}
	id, err := envelope.ID.Int64()
	if err != nil {
		return
	}
	if envelope.Error != nil {
		// The app-server's error text is provider-controlled on a path that
		// carries OAuth state, so the numeric code is what surfaces. It is
		// enough to tell a wrong-shape request from a refused one.
		s.resolve(id, rpcOutcome{err: fmt.Errorf("codex: sign-in error %d", envelope.Error.Code)})
		return
	}
	s.resolve(id, rpcOutcome{result: envelope.Result})
}

func (s *LoginSession) resolve(id int64, outcome rpcOutcome) {
	s.mu.Lock()
	answers, ok := s.pending[id]
	delete(s.pending, id)
	s.mu.Unlock()
	if ok {
		answers <- outcome
	}
}

func (s *LoginSession) settle(params json.RawMessage) {
	var payload struct {
		LoginID *string `json:"loginId"`
		Success bool    `json:"success"`
		Error   *string `json:"error"`
	}
	if err := json.Unmarshal(params, &payload); err != nil || payload.LoginID == nil {
		return
	}
	outcome := loginOutcome{success: payload.Success}
	if payload.Error != nil {
		outcome.message = *payload.Error
	}
	s.mu.Lock()
	answers, ok := s.waiters[*payload.LoginID]
	if ok {
		delete(s.waiters, *payload.LoginID)
	} else if len(s.settled) < maxSettledLogins {
		s.settled[*payload.LoginID] = outcome
	}
	s.mu.Unlock()
	if ok {
		answers <- outcome
	}
}

func (s *LoginSession) failAll(cause error) {
	if errors.Is(cause, io.EOF) {
		cause = errors.New("codex: the app-server exited before sign-in completed")
	} else {
		cause = fmt.Errorf("codex: sign-in read: %w", cause)
	}
	s.mu.Lock()
	s.failed = cause
	requests := s.pending
	waiters := s.waiters
	s.pending = make(map[int64]chan rpcOutcome)
	s.waiters = make(map[string]chan loginOutcome)
	s.mu.Unlock()
	for _, answers := range requests {
		answers <- rpcOutcome{err: cause}
	}
	for _, answers := range waiters {
		answers <- loginOutcome{err: cause}
	}
}

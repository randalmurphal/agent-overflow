package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"agent-overflow/internal/provider"
)

// Claude sign-in, driven over the CLI's headless control channel rather than
// through `claude auth login`. The shapes, the burned-flow rule, the
// supersede rule and the prose-only error surface are documented once in
// docs/references/claude-wire.md § The sign-in control channel; this file is
// the client for them.
//
// Why not `auth login`: that command owns the browser and the URL, and
// reports completion only by exiting. On a host with no browser the URL was
// unreachable, and on a remote device it was being opened on a machine
// nobody could see. Here AO holds the URLs, so it can open one host-side OR
// show it on the device the person is actually looking at.

// DefaultLoginTimeout is the whole flow's budget. The CLI's own
// wait-for-completion request pends unbounded with no keepalive, so the
// deadline has to be ours; a person who walked away from a half-finished
// browser sign-in leaves the process parked forever otherwise.
const DefaultLoginTimeout = 10 * time.Minute

// loginErrorRunes bounds prose that reaches a user-facing error. The CLI's
// refusals are hand-written sentences, but they are provider output on a
// path this package does not control, and an unbounded one would ride an
// event frame.
const loginErrorRunes = 400

// ErrLoginFlowBurned reports that the CLI no longer holds the flow the
// request named. It is the CLI's "No active claude_authenticate flow", and it
// happens for three reasons that are one reason: a callback was answered with
// a bad code, the loopback listener rejected a request, or a newer
// claude_authenticate replaced this one. In every case the PKCE challenge the
// user is holding a link for is dead, and the only recovery is a fresh flow
// with a fresh link — never a retry of the same one.
var ErrLoginFlowBurned = errors.New("claude: that sign-in link is no longer valid")

// errLoginSuperseded resolves a wait this package abandoned on purpose. The
// CLI never answers a superseded wait, so without an abandon the goroutine
// that made it would sit on its channel until the whole flow's deadline —
// one leaked goroutine per retry, and retries are the common path here.
var errLoginSuperseded = errors.New("claude: sign-in was restarted")

// LoginConfig configures one sign-in process.
type LoginConfig struct {
	Binary string
	// ConfigDir is the isolated CLAUDE_CONFIG_DIR the sign-in writes its
	// credential into. Required: this flow must never land in the canonical
	// home, because the adoption epilogue decides which slot it belongs in.
	ConfigDir string
}

// LoginURLs are the two addresses one flow hands back. They carry the SAME
// PKCE challenge and state and differ only in redirect_uri, so they are two
// ways into one exchange rather than two exchanges.
type LoginURLs struct {
	// ManualURL redirects to the page that shows the user a `code#state`
	// string to paste back. It is the only form that works for a person who
	// is not sitting at this machine.
	ManualURL string
	// AutomaticURL redirects to a loopback listener the CLI binds before it
	// answers, so opening it on THIS host completes the flow with nothing to
	// paste.
	AutomaticURL string
}

// LoginSession is one CLI process held open on its stdin, driving the sign-in
// control channel. Stdin must stay open for the whole flow: EOF ends the
// process in about a second and takes the loopback listener with it.
//
// One flow is live at a time. Starting another supersedes the first, which is
// the CLI's own semantic and the reason the outstanding wait is abandoned
// rather than left to a deadline.
type LoginSession struct {
	proc *provider.Process

	mu      sync.Mutex
	pending map[string]chan controlOutcome
	nextID  uint64
	// waitID is the outstanding claude_oauth_wait_for_completion request for
	// the CURRENT flow, or empty. Tracked so a supersede can abandon exactly
	// that request and nothing else.
	waitID string
	// claudeAI is the loginWithClaudeAi value the live flow uses. It starts
	// true (a Claude subscription is what this surface is for) and flips only
	// when managed settings refuse that account type.
	claudeAI bool
	// failed records the terminal read-loop error, so a call made after the
	// process died answers with the reason instead of hanging.
	failed error

	closeOnce sync.Once
}

// controlOutcome is one answered control_request: the success payload, or the
// CLI's prose.
type controlOutcome struct {
	response json.RawMessage
	err      error
}

// StartLogin spawns the sign-in process and begins reading its control
// channel. Nothing has been authenticated when it returns and nothing
// credential-shaped exists in ConfigDir yet; call Authenticate next.
//
// The caller owns the returned session and MUST Close it.
func StartLogin(ctx context.Context, cfg LoginConfig) (*LoginSession, error) {
	binary := strings.TrimSpace(cfg.Binary)
	if binary == "" {
		binary = "claude"
	}
	if strings.TrimSpace(cfg.ConfigDir) == "" {
		return nil, errors.New("claude: sign-in requires an isolated config directory")
	}
	// Plain cancel (instant SIGKILL) is deliberate, as it was for the old
	// `auth login` spawn: the config dir is always a fresh ephemeral
	// directory, so a kill mid-exchange abandons a brand-new grant. It cannot
	// brick a saved account the way killing an account PROBE can, which is
	// what GracefulCancel exists for.
	proc, err := provider.Spawn(ctx, provider.SpawnConfig{
		Binary:   binary,
		Args:     loginArgs(),
		Env:      map[string]string{"CLAUDE_CONFIG_DIR": cfg.ConfigDir},
		UnsetEnv: []string{"CLAUDE_SECURESTORAGE_CONFIG_DIR"},
		Provider: "claude",
	})
	if err != nil {
		return nil, fmt.Errorf("claude: start sign-in: %w", err)
	}
	session := &LoginSession{
		proc:     proc,
		pending:  make(map[string]chan controlOutcome),
		claudeAI: true,
	}
	go session.readLoop()
	return session, nil
}

// loginArgs is the headless control-channel invocation. Every flag is load
// bearing: stream-json in both directions is what makes control_request a
// thing this process can send, and --verbose is MANDATORY — without it the
// CLI refuses the combination and exits 1 before reading a byte of stdin.
// No prompt is sent, so the process emits nothing and waits.
func loginArgs() []string {
	return []string{
		"-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
	}
}

// Authenticate starts a fresh flow and returns its two URLs. It supersedes
// whatever flow was live, which is what the caller wants on every path that
// reaches it: a first sign-in, a retry after a bad paste, and a restart after
// the loopback listener rejected a request are all "the previous link is
// dead, give me a new one".
func (s *LoginSession) Authenticate(ctx context.Context) (LoginURLs, error) {
	s.supersede()

	claudeAI := s.loginWithClaudeAI()
	urls, err := s.authenticateOnce(ctx, claudeAI)
	if err == nil || !mentionsForcedLoginMethod(err) {
		return urls, err
	}
	// Managed settings pin the account type, and the refusal says so in prose
	// with no machine-readable code beside it. It is raised BEFORE the CLI
	// starts anything — no listener, no challenge, no state — so retrying the
	// other account type is free and cannot strand a half-open flow.
	//
	// The retry's error is what surfaces if it also fails: by then the
	// account type the settings demanded is the one that was tried, so its
	// refusal describes the real blocker, while the first one only said which
	// type to use.
	flipped := !claudeAI
	urls, retryErr := s.authenticateOnce(ctx, flipped)
	if retryErr != nil {
		return LoginURLs{}, retryErr
	}
	s.setLoginWithClaudeAI(flipped)
	return urls, nil
}

func (s *LoginSession) authenticateOnce(ctx context.Context, claudeAI bool) (LoginURLs, error) {
	raw, err := s.call(ctx, map[string]any{
		"subtype":           "claude_authenticate",
		"loginWithClaudeAi": claudeAI,
	})
	if err != nil {
		return LoginURLs{}, err
	}
	var payload struct {
		ManualURL    string `json:"manualUrl"`
		AutomaticURL string `json:"automaticUrl"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return LoginURLs{}, fmt.Errorf("claude: decode sign-in response: %w", err)
	}
	if payload.ManualURL == "" {
		// The manual URL is the one form that reaches a person who is not at
		// this machine, so a response without it cannot serve the remote
		// method and must not be reported as a usable flow.
		return LoginURLs{}, errors.New("claude: sign-in returned no link to open")
	}
	return LoginURLs{ManualURL: payload.ManualURL, AutomaticURL: payload.AutomaticURL}, nil
}

// SubmitCallback answers the live flow with the code and state the user
// pasted back. One failure BURNS the flow — the CLI closes its listener and
// clears its slot, so every later request answers ErrLoginFlowBurned — which
// is why the caller must start a fresh Authenticate rather than re-prompt for
// the same link.
func (s *LoginSession) SubmitCallback(ctx context.Context, code, state string) error {
	code = strings.TrimSpace(code)
	state = strings.TrimSpace(state)
	// Refused here as well as at the call site: the CLI forwards an absent
	// half into its token request as undefined and gets back an opaque
	// "Request failed with status code 400", which burns the flow and tells
	// the user nothing.
	if code == "" || state == "" {
		return errors.New("claude: the pasted code is incomplete")
	}
	// The flow is spent either way once this is answered, so the wait that
	// was riding it is abandoned before the write rather than after: a
	// success resolves through THIS response.
	s.supersede()
	_, err := s.call(ctx, map[string]any{
		"subtype":           "claude_oauth_callback",
		"authorizationCode": code,
		"state":             state,
	})
	return err
}

// WaitForCompletion blocks until the CLI's own loopback listener completes
// the flow, the flow is burned, or ctx expires. It is the browser method's
// completion signal; the remote method completes on SubmitCallback's response
// instead.
//
// The CLI answers this whenever it likes and never keeps it alive, so ctx is
// the only bound. It does not block the CLI's stdin loop, so a supersede can
// still be written while one is outstanding.
func (s *LoginSession) WaitForCompletion(ctx context.Context) error {
	id := s.allocate()
	s.mu.Lock()
	s.waitID = id
	s.mu.Unlock()
	_, err := s.callWithID(ctx, id, map[string]any{
		"subtype": "claude_oauth_wait_for_completion",
	})
	s.mu.Lock()
	if s.waitID == id {
		s.waitID = ""
	}
	s.mu.Unlock()
	return err
}

// Close ends the process. Closing stdin is the clean shutdown the CLI
// documents for this mode (SIGTERM is clean too, and Process.Close escalates
// to it), and nothing credential-shaped has been written unless the flow
// succeeded, so an abandoned sign-in leaves the config dir as it found it.
func (s *LoginSession) Close() error {
	var err error
	s.closeOnce.Do(func() { err = s.proc.Close() })
	return err
}

// supersede abandons the outstanding wait, if there is one. Called before
// anything that ends the live flow.
func (s *LoginSession) supersede() {
	s.mu.Lock()
	id := s.waitID
	s.waitID = ""
	s.mu.Unlock()
	if id != "" {
		s.resolve(id, controlOutcome{err: errLoginSuperseded})
	}
}

func (s *LoginSession) loginWithClaudeAI() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.claudeAI
}

func (s *LoginSession) setLoginWithClaudeAI(value bool) {
	s.mu.Lock()
	s.claudeAI = value
	s.mu.Unlock()
}

func (s *LoginSession) allocate() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	return "ao-login-" + strconv.FormatUint(s.nextID, 10)
}

func (s *LoginSession) call(ctx context.Context, request map[string]any) (json.RawMessage, error) {
	return s.callWithID(ctx, s.allocate(), request)
}

func (s *LoginSession) callWithID(
	ctx context.Context,
	id string,
	request map[string]any,
) (json.RawMessage, error) {
	answers := make(chan controlOutcome, 1)
	s.mu.Lock()
	if s.failed != nil {
		err := s.failed
		s.mu.Unlock()
		return nil, err
	}
	s.pending[id] = answers
	s.mu.Unlock()

	line, err := json.Marshal(map[string]any{
		"type":       "control_request",
		"request_id": id,
		"request":    request,
	})
	if err != nil {
		s.forget(id)
		return nil, fmt.Errorf("claude: encode sign-in request: %w", err)
	}
	if err := s.proc.WriteLine(line); err != nil {
		s.forget(id)
		return nil, fmt.Errorf("claude: sign-in write: %w", err)
	}

	select {
	case <-ctx.Done():
		s.forget(id)
		return nil, fmt.Errorf("claude: sign-in: %w", ctx.Err())
	case outcome := <-answers:
		return outcome.response, outcome.err
	}
}

func (s *LoginSession) forget(id string) {
	s.mu.Lock()
	delete(s.pending, id)
	if s.waitID == id {
		s.waitID = ""
	}
	s.mu.Unlock()
}

func (s *LoginSession) resolve(id string, outcome controlOutcome) {
	s.mu.Lock()
	answers, ok := s.pending[id]
	delete(s.pending, id)
	s.mu.Unlock()
	if ok {
		answers <- outcome
	}
}

// readLoop owns every read from the process for its whole life. One goroutine
// rather than a read-per-call because a wait_for_completion can be
// outstanding while an unrelated request is answered, and two readers on one
// pipe would hand each other's lines to the wrong waiter.
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
	var envelope struct {
		Type     string `json:"type"`
		Response struct {
			Subtype   string          `json:"subtype"`
			RequestID string          `json:"request_id"`
			Response  json.RawMessage `json:"response"`
			Error     string          `json:"error"`
		} `json:"response"`
	}
	if err := json.Unmarshal(line, &envelope); err != nil || envelope.Type != "control_response" {
		// This process is sent no prompt, so it emits nothing else — but a
		// CLI release adding a line here must not break a live sign-in, the
		// same log-and-continue rule the session parser follows.
		return
	}
	id := envelope.Response.RequestID
	if id == "" {
		return
	}
	if envelope.Response.Subtype == "success" {
		s.resolve(id, controlOutcome{response: envelope.Response.Response})
		return
	}
	s.resolve(id, controlOutcome{err: loginError(envelope.Response.Error)})
}

// failAll ends every outstanding request when the pipe dies, and latches the
// reason so a later call answers instead of parking on a channel nothing will
// ever write to.
func (s *LoginSession) failAll(cause error) {
	if errors.Is(cause, io.EOF) {
		cause = errors.New("claude: the sign-in process exited")
	} else {
		cause = fmt.Errorf("claude: sign-in read: %w", cause)
	}
	s.mu.Lock()
	s.failed = cause
	waiting := s.pending
	s.pending = make(map[string]chan controlOutcome)
	s.waitID = ""
	s.mu.Unlock()
	for _, answers := range waiting {
		answers <- controlOutcome{err: cause}
	}
}

// loginError turns the CLI's error prose into an error. There is no
// machine-readable code on this wire, so the ONE string matched is the
// burned-flow sentence — everything else is surfaced verbatim, because these
// refusals are hand-written sentences naming a setting or a status code and
// replacing them with "sign-in failed" would leave the user nothing to act
// on. Bounded and single-lined, since it is provider output riding an event
// frame to a user's screen.
func loginError(message string) error {
	text := strings.Join(strings.Fields(message), " ")
	if text == "" {
		return errors.New("claude: sign-in failed")
	}
	if strings.Contains(text, "No active claude_authenticate flow") {
		return ErrLoginFlowBurned
	}
	if len([]rune(text)) > loginErrorRunes {
		text = boundRunes(text, loginErrorRunes) + "…"
	}
	return errors.New(text)
}

// mentionsForcedLoginMethod identifies the managed-settings refusal. The
// setting's name is the only stable token in a sentence whose wording is the
// CLI's to change, so the match is on that and nothing else.
func mentionsForcedLoginMethod(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "forceloginmethod")
}

package provideraccountapp

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"agent-overflow/internal/externalurl"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/provider/codex"
)

// This file owns one live sign-in per provider.
//
// A provider sign-in cannot be one blocking call, because the person finishing
// it may not be at this machine: the link has to reach them, and whatever they
// send back has to reach the flow that is still holding it open. So a sign-in
// is a SESSION here — started, observed, answered and cancelled through four
// fast calls, with every transition pushed as state. The preparation and the
// adoption that surround it are login.go's.

// LoginPhase is where one sign-in has got to. Every phase names something for
// the user to do or to wait for, never a step of ours, because this is what
// the UI branches on.
type LoginPhase string

const (
	// LoginPhaseIdle is no sign-in running, which is what a provider that has
	// never started one reports. It is also what a cancel leaves behind.
	LoginPhaseIdle LoginPhase = "idle"
	// LoginPhaseStarting is the provider process coming up. It is brief, and
	// exists so a slow spawn is visibly a sign-in rather than a dead button.
	LoginPhaseStarting LoginPhase = "starting"
	// LoginPhaseAwaitingBrowser is a browser open on this machine, finishing
	// on its own. Nothing is asked of a remote reader.
	LoginPhaseAwaitingBrowser LoginPhase = "awaiting_browser"
	// LoginPhaseAwaitingCode is a code standing between us and done: either
	// one the user types into the provider's page (Codex, in UserCode) or one
	// the provider shows them to paste back here (Claude).
	LoginPhaseAwaitingCode LoginPhase = "awaiting_code"
	// LoginPhaseVerifying is the provider answering and the credential being
	// adopted. It can take seconds; it asks nothing of the user.
	LoginPhaseVerifying LoginPhase = "verifying"
	LoginPhaseSucceeded LoginPhase = "succeeded"
	LoginPhaseFailed    LoginPhase = "failed"
)

// LoginMethod is where the sign-in page opens. The CLIENT chooses: only it
// knows whether the person reading the screen is at this machine. The backend
// changes the choice in exactly one case — a browser method on a host with no
// opener, which degrades to remote rather than losing the link.
type LoginMethod string

const (
	LoginMethodBrowser LoginMethod = "browser"
	LoginMethodRemote  LoginMethod = "remote"
)

// LoginState is the whole of what a client knows about a sign-in. It is
// pushed on every transition and readable at any time, so a client that
// reconnects mid-flow rejoins by reading it rather than by starting again.
type LoginState struct {
	Provider string      `json:"provider"`
	Phase    LoginPhase  `json:"phase"`
	Method   LoginMethod `json:"method,omitempty"`
	// AuthorizeURL is the page to open, and it is always the one that works
	// for this Method: the loopback-completing link for a browser on this
	// machine, the paste-back or device page for anywhere else.
	AuthorizeURL string `json:"authorizeUrl,omitempty"`
	// UserCode is the Codex device code, typed by hand on another screen.
	// Claude's remote flow has no counterpart — its code travels the other
	// way, from the page back to us.
	UserCode string `json:"userCode,omitempty"`
	// Error is the one prose slot, and the phase says how to read it: on
	// `failed` it is why the sign-in ended, and on any other phase it is what
	// just changed and why (a burned link replaced, a browser that could not
	// be opened here). Both are sentences for the user.
	Error string `json:"error,omitempty"`
	// StartedAt and ExpiresAt are epoch milliseconds. ExpiresAt is when this
	// flow stops being answerable, which for a device code is the code's own
	// life and elsewhere is our budget.
	StartedAt int64 `json:"startedAt,omitempty"`
	ExpiresAt int64 `json:"expiresAt,omitempty"`
}

// IdleLoginState is what a provider with no sign-in reports. Callers build it
// rather than the zero value so the phase is always a word the client can
// switch on.
func IdleLoginState(providerName string) LoginState {
	return LoginState{Provider: providerName, Phase: LoginPhaseIdle}
}

// defaultLoginBudget is how long a sign-in stays answerable when the provider
// does not set the clock itself. Claude's own wait pends unbounded with no
// keepalive, so the deadline has to be ours.
const defaultLoginBudget = 10 * time.Minute

// loginBudget is how long one run may stay open: the longest any of that
// provider's flows can need, not the one it started with. A run can change
// method mid-flight — a host with no browser opener degrades to the device
// code — and a deadline cut short of a code's own life would kill a link the
// UI is still showing as good.
func loginBudget(providerName string) time.Duration {
	if providerName == string(provider.Codex) {
		return max(defaultLoginBudget, codex.DeviceCodeLifetime)
	}
	return defaultLoginBudget
}

// maxLoginFlows bounds how many times one run replaces a burned link before
// giving up. A burn needs a user action, so this is not reachable by normal
// use; it is here because a provider that burned every flow on sight would
// otherwise spin.
const maxLoginFlows = 5

// The sentences the coordinator itself puts in front of the user. Provider
// prose is surfaced as the provider wrote it; these cover what only we know.
const (
	loginNoOpenerNotice = "This machine has no browser to open, " +
		"so finish signing in on the device you are reading this on."
	loginBurnedNotice = "That link stopped working, so here is a new one. " +
		"The code from the previous link will not be accepted."
	loginTimedOutNotice  = "The sign-in ran out of time before it finished."
	loginCancelledNotice = "The sign-in was cancelled."
)

// loginProseRunes bounds a provider's own message the same way the drivers
// bound theirs: long enough for a real explanation, short enough that a page
// of output cannot become the UI.
const loginProseRunes = 400

var (
	errNoLoginInProgress = errors.New(
		"there is no sign-in waiting for a code; start signing in again",
	)
	errLoginCodeEmpty = errors.New(
		"enter the code from the sign-in page",
	)
	errLoginCodeIncomplete = errors.New(
		"that is not the whole code; copy all of it, including the part after the # sign",
	)
	errLoginCodeBusy = errors.New(
		"that code is already being checked",
	)
	errLoginCodeUnsupported = errors.New(
		"Codex sign-in finishes on the ChatGPT page, not with a pasted code",
	)
)

// loginFlow is one started flow: what the user must be shown, and the handle
// that settles it. A run goes through more than one when the provider burns
// the link the user is holding — a fresh flow replaces it in place, so they
// get a working link instead of an error for something they did not do.
type loginFlow struct {
	method       LoginMethod
	phase        LoginPhase
	authorizeURL string
	userCode     string
	// loginID correlates the completion notification on Codex, whose wire
	// reports a superseded login's failure alongside the live one's success.
	loginID   string
	expiresAt time.Time
}

// loginRun is one live sign-in. Exactly one exists per provider: two flows
// would race for the same canonical credential slot, and the user can only be
// looking at one link.
//
// Its mutable fields are the registry's, guarded by Manager.loginMu — the run
// goroutine is not their only writer once a paste or a cancel can arrive.
type loginRun struct {
	provider  string
	attempt   *loginAttempt
	cancel    context.CancelFunc
	done      chan struct{}
	pastes    chan loginPaste
	startedAt time.Time
	deadline  time.Time

	claude *claude.LoginSession
	codex  *codex.LoginSession

	flow loginFlow
}

type loginPaste struct {
	code  string
	state string
}

// StartProviderLogin begins a sign-in and returns as soon as there is
// something to show. Whatever was running for this provider is cancelled: the
// user asking again is the user saying the link they were looking at is not
// the one they want.
func (m *Manager) StartProviderLogin(providerName string, method LoginMethod) (LoginState, error) {
	if m.shuttingDown() {
		return IdleLoginState(providerName), m.shutdownError()
	}
	if err := ValidateProvider(providerName); err != nil {
		return IdleLoginState(providerName), err
	}
	if !m.available() {
		return IdleLoginState(providerName), errors.New("provider account storage is unavailable")
	}
	if method != LoginMethodBrowser && method != LoginMethodRemote {
		return IdleLoginState(providerName), fmt.Errorf(
			"sign-in method must be %q or %q",
			LoginMethodBrowser,
			LoginMethodRemote,
		)
	}

	m.CancelProviderLogin(providerName)

	attempt, err := m.beginLogin(providerName)
	if err != nil {
		return m.publishTerminalLogin(providerName, method, err), err
	}

	startedAt := time.Now()
	run := &loginRun{
		provider:  providerName,
		attempt:   attempt,
		done:      make(chan struct{}),
		pastes:    make(chan loginPaste, 1),
		startedAt: startedAt,
		deadline:  startedAt.Add(loginBudget(providerName)),
	}
	run.flow = loginFlow{method: method, phase: LoginPhaseStarting, expiresAt: run.deadline}
	ctx, cancel := context.WithDeadline(m.context(), run.deadline)
	run.cancel = cancel

	m.registerLoginRun(run)
	m.publishLoginFlow(run, run.flow, "")

	flow, notice, err := m.openLoginFlow(ctx, run, method, "")
	if err != nil {
		joined := errors.Join(err, run.teardown())
		m.settleLoginRun(run, LoginPhaseFailed, loginProse(joined))
		return m.ProviderLoginState(providerName), joined
	}
	state := m.publishLoginFlow(run, flow, notice)
	go m.driveLogin(ctx, run)
	return state, nil
}

// ProviderLoginState is the current sign-in state, and never fails: a
// provider that has never signed in is idle. A terminal state is retained
// after its run ends so a client that reconnects across the last transition
// still learns how it went.
func (m *Manager) ProviderLoginState(providerName string) LoginState {
	if err := ValidateProvider(providerName); err != nil {
		return IdleLoginState(providerName)
	}
	m.loginMu.Lock()
	defer m.loginMu.Unlock()
	if state, ok := m.loginStates[providerName]; ok {
		return state
	}
	return IdleLoginState(providerName)
}

// SubmitProviderLoginCode hands Claude's paste-back code to the flow holding
// it open. It returns as soon as the code is handed over; whether the provider
// accepted it arrives as state.
func (m *Manager) SubmitProviderLoginCode(providerName, pasted string) (LoginState, error) {
	if err := ValidateProvider(providerName); err != nil {
		return IdleLoginState(providerName), err
	}
	if providerName != string(provider.Claude) {
		return m.ProviderLoginState(providerName), errLoginCodeUnsupported
	}
	// Refuse a half-empty paste before the CLI sees it: one wrong callback
	// ends the CLI's flow outright, so an obvious mistake must not cost the
	// user their link.
	code, state, err := splitLoginPaste(pasted)
	if err != nil {
		return m.ProviderLoginState(providerName), err
	}

	m.loginMu.Lock()
	run := m.logins[providerName]
	m.loginMu.Unlock()
	if run == nil {
		return m.ProviderLoginState(providerName), errNoLoginInProgress
	}
	select {
	case run.pastes <- loginPaste{code: code, state: state}:
	default:
		return m.ProviderLoginState(providerName), errLoginCodeBusy
	}
	return m.publishLoginPhase(run, LoginPhaseVerifying, ""), nil
}

// CancelProviderLogin ends whatever is running for this provider and leaves
// it idle. It waits for the run to finish so the temporary home is gone before
// the next sign-in cuts its own.
func (m *Manager) CancelProviderLogin(providerName string) LoginState {
	m.loginMu.Lock()
	run := m.logins[providerName]
	if run != nil {
		delete(m.logins, providerName)
	}
	m.loginMu.Unlock()
	if run != nil {
		run.cancel()
		<-run.done
	}
	// Idle even when nothing was running: a client dismissing the panel over a
	// finished sign-in is telling us the retained outcome has been read, and
	// leaving it would put a stale success or failure in front of whoever
	// opens the surface next.
	idle := IdleLoginState(providerName)
	m.storeLoginState(idle)
	m.publishLoginState(idle)
	return idle
}

// ShutdownProviderLogins ends every live sign-in. Nothing is published: the
// process is going away, and a state nobody can read is noise in the log.
func (m *Manager) ShutdownProviderLogins() {
	m.loginMu.Lock()
	runs := make([]*loginRun, 0, len(m.logins))
	for providerName, run := range m.logins {
		runs = append(runs, run)
		delete(m.logins, providerName)
	}
	m.loginMu.Unlock()
	for _, run := range runs {
		run.cancel()
		<-run.done
	}
}

// driveLogin is the rest of one sign-in: wait for the flow to settle, replace
// a link the provider burned, and adopt the credential once it lands.
func (m *Manager) driveLogin(ctx context.Context, run *loginRun) {
	defer close(run.done)
	err := m.runLogin(ctx, run)
	cleanupErr := run.teardown()
	if err != nil {
		m.settleLoginRun(run, LoginPhaseFailed, loginProse(errors.Join(err, cleanupErr)))
		return
	}
	if cleanupErr != nil {
		// The account is switched and active. A leftover temporary home is
		// the boot sweep's problem, not this user's.
		log.Printf("provideraccountapp: %v", cleanupErr)
	}
	m.settleLoginRun(run, LoginPhaseSucceeded, "")
}

func (m *Manager) runLogin(ctx context.Context, run *loginRun) error {
	flow := m.currentLoginFlow(run)
	for range maxLoginFlows {
		notice, err := m.settleLoginFlow(ctx, run, flow)
		if err != nil {
			return err
		}
		if notice == "" {
			m.publishLoginPhase(run, LoginPhaseVerifying, "")
			_, err := m.adoptLogin(run.attempt)
			return err
		}
		next, carried, err := m.openLoginFlow(ctx, run, flow.method, notice)
		if err != nil {
			return err
		}
		flow = next
		m.publishLoginFlow(run, flow, carried)
	}
	return errors.New("the provider ended every sign-in link it produced")
}

// openLoginFlow spawns the provider's sign-in process the first time and asks
// it for a link every time. The returned notice is what to say alongside that
// link, which is not always nothing: a host with no browser opener degrades to
// the remote presentation here rather than discarding the URL.
func (m *Manager) openLoginFlow(
	ctx context.Context,
	run *loginRun,
	method LoginMethod,
	notice string,
) (loginFlow, string, error) {
	switch run.provider {
	case string(provider.Claude):
		return m.openClaudeLoginFlow(ctx, run, method, notice)
	case string(provider.Codex):
		return m.openCodexLoginFlow(ctx, run, method, notice)
	}
	return loginFlow{}, "", fmt.Errorf("provider %q has no sign-in flow", run.provider)
}

func (m *Manager) openClaudeLoginFlow(
	ctx context.Context,
	run *loginRun,
	method LoginMethod,
	notice string,
) (loginFlow, string, error) {
	if run.claude == nil {
		session, err := claude.StartLogin(ctx, claude.LoginConfig{
			Binary:    run.attempt.binary,
			ConfigDir: run.attempt.home.Path,
		})
		if err != nil {
			return loginFlow{}, "", err
		}
		run.claude = session
	}
	urls, err := run.claude.Authenticate(ctx)
	if err != nil {
		return loginFlow{}, "", err
	}
	flow := loginFlow{method: method, expiresAt: run.deadline}
	if method == LoginMethodBrowser {
		// The automatic link is the one a browser HERE can finish on its own:
		// its redirect lands on a loopback listener this CLI just bound. It
		// is also the copyable fallback, because "the opener failed" and "the
		// user opens it themselves" have the same answer.
		err := m.openLoginURL(ctx, urls.AutomaticURL)
		if err == nil {
			flow.phase = LoginPhaseAwaitingBrowser
			flow.authorizeURL = urls.AutomaticURL
			return flow, notice, nil
		}
		if !errors.Is(err, externalurl.ErrNoOpener) {
			return loginFlow{}, "", err
		}
		// No opener means no loopback either: whoever is reading this is not
		// at a browser on this machine, so the automatic link cannot complete
		// for them. The manual one can.
		flow.method = LoginMethodRemote
		notice = joinLoginNotice(notice, loginNoOpenerNotice)
	}
	flow.phase = LoginPhaseAwaitingCode
	flow.authorizeURL = urls.ManualURL
	return flow, notice, nil
}

func (m *Manager) openCodexLoginFlow(
	ctx context.Context,
	run *loginRun,
	method LoginMethod,
	notice string,
) (loginFlow, string, error) {
	if run.codex == nil {
		session, err := codex.StartLogin(ctx, codex.LoginConfig{
			Binary: run.attempt.binary,
			Env:    map[string]string{"CODEX_HOME": run.attempt.home.Path},
		})
		if err != nil {
			return loginFlow{}, "", err
		}
		run.codex = session
	}
	flow := loginFlow{method: method}
	if method == LoginMethodBrowser {
		start, err := run.codex.StartBrowserLogin(ctx)
		if err != nil {
			return loginFlow{}, "", err
		}
		openErr := m.openLoginURL(ctx, start.AuthURL)
		if openErr == nil {
			flow.phase = LoginPhaseAwaitingBrowser
			flow.authorizeURL = start.AuthURL
			flow.loginID = start.LoginID
			flow.expiresAt = run.deadline
			return flow, notice, nil
		}
		if !errors.Is(openErr, externalurl.ErrNoOpener) {
			return loginFlow{}, "", openErr
		}
		// This link completes on a loopback listener this app-server bound,
		// so showing it to another device would produce a page that can never
		// come back. The device code is the flow that can. One login per
		// process, so the browser one is cancelled before the device one
		// starts.
		if err := run.codex.Cancel(ctx, start.LoginID); err != nil {
			return loginFlow{}, "", err
		}
		flow.method = LoginMethodRemote
		notice = joinLoginNotice(notice, loginNoOpenerNotice)
	}
	start, err := run.codex.StartDeviceLogin(ctx)
	if err != nil {
		return loginFlow{}, "", err
	}
	flow.phase = LoginPhaseAwaitingCode
	flow.authorizeURL = start.VerificationURL
	flow.userCode = start.UserCode
	flow.loginID = start.LoginID
	// The code's own life is the provider's fact, and the run's deadline is
	// ours; the user is shown whichever runs out first.
	flow.expiresAt = run.deadline
	if !start.ExpiresAt.IsZero() && start.ExpiresAt.Before(run.deadline) {
		flow.expiresAt = start.ExpiresAt
	}
	return flow, notice, nil
}

// settleLoginFlow blocks until this flow ends. A non-empty notice means the
// sign-in is still alive but the link the user is holding is not: the caller
// opens a fresh one and shows the notice with it.
func (m *Manager) settleLoginFlow(
	ctx context.Context,
	run *loginRun,
	flow loginFlow,
) (string, error) {
	switch run.provider {
	case string(provider.Claude):
		if flow.method == LoginMethodBrowser {
			err := run.claude.WaitForCompletion(ctx)
			if errors.Is(err, claude.ErrLoginFlowBurned) {
				// The CLI's own listener rejects the whole flow on a callback
				// it cannot match, so a browser sign-in can burn without the
				// user ever touching this app.
				return loginBurnedNotice, nil
			}
			return "", err
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case paste := <-run.pastes:
			err := run.claude.SubmitCallback(ctx, paste.code, paste.state)
			if err == nil {
				return "", nil
			}
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			// One rejected callback ends the CLI's flow: its listener closes
			// and everything after answers that no flow is active. So the
			// only way forward is a fresh link, and the user gets it without
			// having to ask.
			return joinLoginNotice(loginProse(err), loginBurnedNotice), nil
		}
	case string(provider.Codex):
		return "", run.codex.WaitForCompletion(ctx, flow.loginID)
	}
	return "", fmt.Errorf("provider %q has no sign-in flow", run.provider)
}

func (m *Manager) openLoginURL(ctx context.Context, rawURL string) error {
	if m.deps.OpenBrowser == nil {
		return fmt.Errorf("%w (this build opens no links)", externalurl.ErrNoOpener)
	}
	return m.deps.OpenBrowser(ctx, rawURL)
}

// teardown closes the provider process and removes the temporary home. Both
// halves run whatever the other does, because a live app-server and a stray
// home are different problems.
func (r *loginRun) teardown() error {
	var errs []error
	if r.claude != nil {
		errs = append(errs, r.claude.Close())
	}
	if r.codex != nil {
		errs = append(errs, r.codex.Close())
	}
	r.cancel()
	errs = append(errs, r.attempt.cleanup())
	return errors.Join(errs...)
}

// splitLoginPaste splits Claude's paste-back value on the FIRST `#`. The state
// half is opaque and can contain anything the provider put there, so only the
// first separator is a separator.
func splitLoginPaste(pasted string) (code, state string, err error) {
	trimmed := strings.TrimSpace(pasted)
	if trimmed == "" {
		return "", "", errLoginCodeEmpty
	}
	code, state, found := strings.Cut(trimmed, "#")
	code = strings.TrimSpace(code)
	state = strings.TrimSpace(state)
	if !found || code == "" || state == "" {
		return "", "", errLoginCodeIncomplete
	}
	return code, state, nil
}

// loginProse turns an error into the sentence the user reads. Provider prose
// is surfaced as the provider wrote it — a refusal naming a managed setting or
// a rejected code says more than anything we could substitute — bounded so a
// page of output cannot become the UI.
func loginProse(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.DeadlineExceeded):
		return loginTimedOutNotice
	case errors.Is(err, context.Canceled):
		return loginCancelledNotice
	}
	message := strings.Join(strings.Fields(err.Error()), " ")
	runes := []rune(message)
	if len(runes) > loginProseRunes {
		return string(runes[:loginProseRunes]) + "…"
	}
	return message
}

func joinLoginNotice(first, second string) string {
	switch {
	case first == "":
		return second
	case second == "":
		return first
	}
	return first + " " + second
}

// ---- registry ----------------------------------------------------------
//
// loginMu guards the per-provider run pointers, their presentation fields and
// the retained states. It is a leaf: nothing below it takes another Manager
// lock, and every publish happens after it is released.

func (m *Manager) registerLoginRun(run *loginRun) {
	m.loginMu.Lock()
	defer m.loginMu.Unlock()
	if m.logins == nil {
		m.logins = make(map[string]*loginRun, 2)
	}
	m.logins[run.provider] = run
}

func (m *Manager) currentLoginFlow(run *loginRun) loginFlow {
	m.loginMu.Lock()
	defer m.loginMu.Unlock()
	return run.flow
}

// publishLoginFlow records what the user is now being shown and pushes it. A
// run that has been superseded or cancelled publishes nothing: its successor
// owns the state, and a late frame from a dead flow would show a link that no
// longer answers.
func (m *Manager) publishLoginFlow(run *loginRun, flow loginFlow, notice string) LoginState {
	m.loginMu.Lock()
	if m.logins[run.provider] != run {
		state := m.loginStates[run.provider]
		m.loginMu.Unlock()
		return state
	}
	run.flow = flow
	state := run.stateLocked(flow.phase, notice)
	m.storeLoginStateLocked(state)
	m.loginMu.Unlock()
	m.publishLoginState(state)
	return state
}

func (m *Manager) publishLoginPhase(run *loginRun, phase LoginPhase, notice string) LoginState {
	m.loginMu.Lock()
	if m.logins[run.provider] != run {
		state := m.loginStates[run.provider]
		m.loginMu.Unlock()
		return state
	}
	state := run.stateLocked(phase, notice)
	m.storeLoginStateLocked(state)
	m.loginMu.Unlock()
	m.publishLoginState(state)
	return state
}

// settleLoginRun retires a run and publishes its outcome in one step, so a
// second sign-in cannot start against a run that is already finished while its
// last frame is still being written.
func (m *Manager) settleLoginRun(run *loginRun, phase LoginPhase, notice string) {
	m.loginMu.Lock()
	if m.logins[run.provider] != run {
		m.loginMu.Unlock()
		return
	}
	delete(m.logins, run.provider)
	state := run.stateLocked(phase, notice)
	m.storeLoginStateLocked(state)
	m.loginMu.Unlock()
	m.publishLoginState(state)
}

// publishTerminalLogin records a failure that happened before there was a run
// to hang it on, which is how a caller learns why nothing started.
func (m *Manager) publishTerminalLogin(providerName string, method LoginMethod, err error) LoginState {
	state := LoginState{
		Provider: providerName,
		Phase:    LoginPhaseFailed,
		Method:   method,
		Error:    loginProse(err),
	}
	m.storeLoginState(state)
	m.publishLoginState(state)
	return state
}

func (r *loginRun) stateLocked(phase LoginPhase, notice string) LoginState {
	state := LoginState{
		Provider:  r.provider,
		Phase:     phase,
		Method:    r.flow.method,
		Error:     notice,
		StartedAt: r.startedAt.UnixMilli(),
	}
	if !r.flow.expiresAt.IsZero() {
		state.ExpiresAt = r.flow.expiresAt.UnixMilli()
	}
	// A finished sign-in shows no link: succeeding makes it spent, and
	// failing makes it wrong.
	switch phase {
	case LoginPhaseSucceeded, LoginPhaseFailed:
	default:
		state.AuthorizeURL = r.flow.authorizeURL
		state.UserCode = r.flow.userCode
	}
	return state
}

func (m *Manager) storeLoginState(state LoginState) {
	m.loginMu.Lock()
	defer m.loginMu.Unlock()
	m.storeLoginStateLocked(state)
}

func (m *Manager) storeLoginStateLocked(state LoginState) {
	if m.loginStates == nil {
		m.loginStates = make(map[string]LoginState, 2)
	}
	m.loginStates[state.Provider] = state
}

func (m *Manager) publishLoginState(state LoginState) {
	if m.deps.Accounts != nil {
		m.deps.Accounts.PublishLogin(state)
	}
}

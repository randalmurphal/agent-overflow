package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"strings"
	"time"

	"agent-overflow/internal/aocli"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/sessionruntime"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadmode"
	"agent-overflow/internal/transport"
	"agent-overflow/internal/workflow/def"
	"agent-overflow/internal/workflow/engine"
)

// sessionManager is a root naming adapter over the package-owned runtime. It
// holds no state; sessionruntime.Manager is the sole authority.
type sessionManager struct{ runtime *sessionruntime.Manager }

func (a *App) sessionManager() sessionManager {
	a.sessionRuntimeOnce.Do(func() { a.sessionRuntime = sessionruntime.New() })
	return sessionManager{runtime: a.sessionRuntime}
}

func (m sessionManager) get(threadID string) (session, bool)  { return m.runtime.Get(threadID) }
func (m sessionManager) put(threadID string, entry session)   { m.runtime.Put(threadID, entry) }
func (m sessionManager) take(threadID string) (session, bool) { return m.runtime.Take(threadID) }
func (m sessionManager) unregister(threadID, token string) (session, bool) {
	return m.runtime.Unregister(threadID, token)
}
func (m sessionManager) beginStart(threadID string) (*sessionStart, bool) {
	return m.runtime.BeginStart(threadID)
}
func (m sessionManager) finishStart(threadID string, start *sessionStart) {
	m.runtime.FinishStart(threadID, start)
}
func (m sessionManager) startState(threadID string) (*sessionStart, bool) {
	return m.runtime.StartState(threadID)
}
func (m sessionManager) updateLaunchOpts(threadID, token string, options provider.SessionOptions) {
	m.runtime.UpdateLaunchOptions(threadID, token, options)
}
func (m sessionManager) updateCredentials(threadID, token string, generation uint64, accountID string, account provider.AccountInfo) {
	m.runtime.UpdateCredentials(threadID, token, generation, accountID, account)
}
func (m sessionManager) updateProviderCredentials(providerName string, generation uint64, accountID string, account provider.AccountInfo) []string {
	return m.runtime.UpdateProviderCredentials(providerName, generation, accountID, account)
}
func (m sessionManager) recordActivity(threadID, token string, kind provider.EventKind, content string, now time.Time) {
	m.runtime.RecordActivity(threadID, token, kind, content, now)
}
func (m sessionManager) anyCodexSession() (*codex.Session, string) {
	return m.runtime.AnyCodexSession()
}
func (m sessionManager) codexSessionForBinary(binary string) *codex.Session {
	return m.runtime.CodexSessionForBinary(binary)
}

func (m sessionManager) browserMCPSessions() []sessionruntime.BrowserMCPSession {
	return m.runtime.BrowserMCPSessions()
}

func (m sessionManager) threadIDsForProviderOrStarting(providerName string) []string {
	return m.runtime.ThreadIDsForProviderOrStarting(providerName)
}
func (m sessionManager) idleCandidates(cutoffNano int64) []string {
	return m.runtime.IdleCandidates(cutoffNano)
}
func (m sessionManager) takeIdle(threadID string, cutoffNano int64) (session, bool) {
	return m.runtime.TakeIdle(threadID, cutoffNano)
}
func (m sessionManager) snapshot() map[string]session         { return m.runtime.Snapshot() }
func (m sessionManager) snapshotAndClear() map[string]session { return m.runtime.SnapshotAndClear() }

type sessionStart = sessionruntime.Start

// runSessionStart single-flights session starts per thread: the first caller
// leads and runs start(), everybody else joins its result.
//
// Two properties are load-bearing and were both learned the hard way:
//
//   - The leader's teardown is DEFERRED. A panic inside start() would
//     otherwise leave the in-flight entry registered and its `done` channel
//     open forever, so every later start of that thread — and every joiner
//     already parked — blocks for the life of the process.
//   - A joiner's wait is cancellable. It has performed no side effects, so
//     abandoning it costs nothing; the leader is unaffected and its start
//     runs to completion either way. (The leader's own wait is the provider
//     IO inside start(), which deliberately stays uncancellable — a
//     cancelled spawn/send is indistinguishable from a delivered one.)
func (a *App) runSessionStart(ctx context.Context, threadID string, start func() error) error {
	startState, leader := a.sessionManager().beginStart(threadID)
	if !leader {
		select {
		case <-startState.Done:
			return startState.Err
		case <-ctx.Done():
			return fmt.Errorf("waiting for in-flight session start of thread %s: %w", threadID, ctx.Err())
		}
	}

	completed := false
	defer func() {
		if !completed && startState.Err == nil {
			// The leader is unwinding through a panic. Joiners are released
			// with a real cause instead of a nil error plus a thread that
			// mysteriously has no session.
			startState.Err = fmt.Errorf("session start for thread %s panicked", threadID)
		}
		a.sessionManager().finishStart(threadID, startState)
	}()
	startState.Err = start()
	completed = true
	return startState.Err
}

func (a *App) closeProviderSession(threadID string, sess session) error {
	providerSess := sess.ProviderSession()
	if providerSess == nil {
		return nil
	}
	// Capture the pgid before Close — the process exits during Close and
	// we want the group id regardless.
	pgid := providerSess.PID()
	if err := providerSess.Close(); err != nil {
		return fmt.Errorf("close %s session for thread %s: %w", sess.Provider, threadID, err)
	}
	// Clean close → the subprocess is down, so stop the orphan reaper from
	// tracking it. On a Close error we deliberately keep the watch: an
	// abandoned-but-still-alive subprocess must still be reaped if the app
	// later dies.
	a.releaseSessionProcess(pgid)
	return nil
}

// The `ao` execution surface (spec §5, D15/D17).
//
// Every provider session the app starts is handed a scoped credential in its
// process environment. The credential authorizes the workflow CLI's method set
// and nothing else, and it lives exactly as long as the session: minted while
// the session's launch config is built, registered when the session enters the
// session map, revoked the moment it leaves. Registration riding the session
// map — not the spawn path — is what makes leaking an entry structurally
// impossible: this file is the sole root adapter for those Manager mutations.

// The AO_* variable names are owned by internal/aocli — the reader of the
// contract — so the writer here cannot drift from it. See that package's
// AGENTS.md for the contract itself.

// aoSessionCredential is one minted-but-not-yet-registered credential plus the
// env it renders to. It is carried from launch-config assembly to session
// registration; a spawn that fails never registers, so the token dies unused.
type aoSessionCredential struct {
	token string
	scope transport.CallerScope
	env   map[string]string
}

// ResolveScopedToken implements transport.ScopedTokens. An unknown or revoked
// token resolves to nothing, which the route turns into a 401.
//
// `//wails:ignore` keeps it off the wire entirely: a method that turns a token
// into the authority it carries must never be callable by anything holding one.
//
//wails:ignore
func (a *App) ResolveScopedToken(token string) (transport.CallerScope, bool) {
	return a.sessionManager().runtime.ResolveAOToken(token)
}

// sessionAOEnv returns the AO_* environment of a thread's live session. Empty
// when the thread has no session — the credential only exists while the process
// that holds it does.
func (a *App) sessionAOEnv(threadID string) map[string]string {
	return a.sessionManager().runtime.AOEnv(threadID)
}

// mintAOCredential builds the scoped credential for a session about to start on
// this thread. A thread the CLI cannot be scoped to (no transport server, no
// project, an unresolvable workflow phase) yields no credential: the session
// starts without AO_* set and `ao` reports that it is not inside a session,
// which is a legible failure rather than a silent partial authority.
func (a *App) mintAOCredential(thread store.Thread) (aoSessionCredential, error) {
	server := a.transportServer.Load()
	if server == nil {
		// Unit-level App instances and non-transport embeddings legitimately
		// omit the HTTP server. Production boot installs it before any session
		// can start.
		return aoSessionCredential{}, nil
	}
	scope, ok, err := a.deriveCallerScope(thread)
	if err != nil {
		return aoSessionCredential{}, err
	}
	if !ok {
		return aoSessionCredential{}, nil
	}
	endpoint, err := aoEndpointFromAppURL(server.AppURL())
	if err != nil {
		return aoSessionCredential{}, err
	}
	token, err := transport.NewToken()
	if err != nil {
		return aoSessionCredential{}, fmt.Errorf("mint ao session token for thread %s: %w", thread.ID, err)
	}
	env := map[string]string{
		aocli.EnvEndpoint: endpoint,
		aocli.EnvToken:    token,
		aocli.EnvThreadID: thread.ID,
	}
	// The project's slug, not its id: the offline half of the CLI resolves
	// project-scoped workflow definitions from a directory named by slug, and it
	// has no app to translate an id with. A scope always carries a project (a
	// projectless thread gets no credential at all), so a missing slug here is a
	// project row that predates slugs rather than a session without a project.
	slug, err := a.projectSlug(scope.ProjectID)
	if err != nil {
		return aoSessionCredential{}, err
	}
	if slug != "" {
		env[aocli.EnvProject] = slug
	}
	if scope.IsPhase() {
		env[aocli.EnvRunID] = scope.ItemID
		env[aocli.EnvPhaseID] = scope.PhaseID
	}
	return aoSessionCredential{token: token, scope: scope, env: env}, nil
}

// projectSlug resolves one project's stable user-facing identifier.
func (a *App) projectSlug(projectID string) (string, error) {
	if strings.TrimSpace(projectID) == "" {
		return "", nil
	}
	project, err := a.store.GetProject(projectID)
	if err != nil {
		return "", fmt.Errorf("ao session env: load project %s: %w", projectID, err)
	}
	return project.Slug, nil
}

// aoEndpointFromAppURL strips the webview's query string (which carries the
// session token) off the transport base URL. The CLI authenticates with its own
// scoped credential and must never be handed the full-authority one.
func aoEndpointFromAppURL(appURL string) (string, error) {
	parsed, err := url.Parse(appURL)
	if err != nil || parsed.Host == "" {
		return "", fmt.Errorf("ao session env: transport URL %q is unusable", appURL)
	}
	parsed.Path = ""
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return strings.TrimSuffix(parsed.String(), "/"), nil
}

// deriveCallerScope answers "what authority does this thread's session have".
// A workflow phase or fan-out unit thread carries its frozen phase's grants; a
// unit inherits the phase's, because a unit is that phase's work. Every other
// app-owned session is interactive: full user authority over the CLI's method
// set, exercised one bash approval at a time.
func (a *App) deriveCallerScope(thread store.Thread) (transport.CallerScope, bool, error) {
	if a.store == nil {
		return transport.CallerScope{}, false, nil
	}
	if thread.Mode == threadmode.ModeWorkflow {
		return a.derivePhaseScope(thread)
	}
	if strings.TrimSpace(thread.ProjectID) == "" {
		// A projectless thread has nothing to scope runs against; the CLI would
		// have no project to start or list in.
		return transport.CallerScope{}, false, nil
	}
	return transport.CallerScope{
		Kind: transport.ScopeKindInteractive, ThreadID: thread.ID, ProjectID: thread.ProjectID,
	}, true, nil
}

// derivePhaseScope resolves the run, phase, and frozen grants behind a workflow
// thread. A unit thread resolves through its unit row; a phase thread through
// its phase row. Either way the grants come from the run's SNAPSHOT, never from
// the definition on disk — a definition edited mid-run must not widen what an
// already-running phase may do.
func (a *App) derivePhaseScope(thread store.Thread) (transport.CallerScope, bool, error) {
	itemID, phaseID, err := a.workflowPhaseForThread(thread.ID)
	if err != nil {
		return transport.CallerScope{}, false, err
	}
	if itemID == "" {
		// A workflow thread whose attempt row is not attached yet (or was torn
		// down) gets no credential rather than an unscoped one.
		return transport.CallerScope{}, false, nil
	}
	item, err := a.store.GetWorkItem(itemID)
	if err != nil {
		return transport.CallerScope{}, false, fmt.Errorf("ao session scope for thread %s: %w", thread.ID, err)
	}
	grants, err := frozenPhaseGrants(item.Snapshot, phaseID)
	if err != nil {
		return transport.CallerScope{}, false, fmt.Errorf("ao session scope for thread %s: %w", thread.ID, err)
	}
	return transport.CallerScope{
		Kind: transport.ScopeKindPhase, ThreadID: thread.ID, ProjectID: item.ProjectID,
		ItemID: item.ID, PhaseID: phaseID, Grants: grants,
	}, true, nil
}

// workflowPhaseForThread maps a workflow thread onto the (item, phase) it is
// running. Unit rows are consulted first: a unit thread also has no phase row of
// its own, so asking the phase table first would attribute it to whichever phase
// attempt last touched the thread.
func (a *App) workflowPhaseForThread(threadID string) (string, string, error) {
	unit, found, err := a.store.GetWorkItemUnitByThread(threadID)
	if err != nil {
		return "", "", err
	}
	if found {
		return unit.ItemID, unit.PhaseID, nil
	}
	phase, found, err := a.store.GetWorkItemPhaseByThread(threadID)
	if err != nil {
		return "", "", err
	}
	if !found {
		return "", "", nil
	}
	return phase.ItemID, phase.PhaseID, nil
}

// frozenPhaseGrants reads one phase's grants out of a run's frozen snapshot. A
// phase the snapshot does not name is a wiring bug, not a permission question,
// so it is reported rather than resolved to "no grants".
func frozenPhaseGrants(snapshot json.RawMessage, phaseID string) ([]string, error) {
	if len(snapshot) == 0 {
		return nil, nil
	}
	var decoded engine.Snapshot
	if err := json.Unmarshal(snapshot, &decoded); err != nil {
		return nil, fmt.Errorf("decode run snapshot: %w", err)
	}
	for _, phase := range decoded.Workflow.Phases {
		if phase.ID != phaseID {
			continue
		}
		grants := make([]string, 0, len(phase.Grants))
		for _, grant := range phase.Grants {
			// A snapshot frozen before a grant was retired (or written by a
			// definition that failed validation on another path) must not hand
			// out authority this build cannot enforce.
			if def.KnownGrant(grant) {
				grants = append(grants, grant)
				continue
			}
			log.Printf("ao session scope: phase %q declares unknown grant %q; ignoring", phaseID, grant)
		}
		return grants, nil
	}
	return nil, fmt.Errorf("phase %q is not in the run's frozen workflow", phaseID)
}

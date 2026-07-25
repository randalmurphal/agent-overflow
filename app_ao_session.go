package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"strings"

	"agent-overflow/internal/aocli"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadmode"
	"agent-overflow/internal/transport"
	"agent-overflow/internal/workflow/def"
	"agent-overflow/internal/workflow/engine"
)

// The `ao` execution surface (spec §5, D15/D17).
//
// Every provider session the app starts is handed a scoped credential in its
// process environment. The credential authorizes the workflow CLI's method set
// and nothing else, and it lives exactly as long as the session: minted while
// the session's launch config is built, registered when the session enters the
// session map, revoked the moment it leaves. Registration riding the session
// map — not the spawn path — is what makes leaking an entry structurally
// impossible: `app_session_manager.go` holds every mutation of that map.

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
	if token == "" {
		return transport.CallerScope{}, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	scope, ok := a.aoTokens[token]
	return scope, ok
}

// registerAOTokenLocked and revokeAOTokenLocked are called with a.mu held, from
// the session-map mutators in app_session_manager.go. They are the ONLY places
// the registry changes.
func (a *App) registerAOTokenLocked(sess session) {
	if sess.aoToken == "" {
		return
	}
	if a.aoTokens == nil {
		a.aoTokens = make(map[string]transport.CallerScope)
	}
	a.aoTokens[sess.aoToken] = sess.aoScope
}

func (a *App) revokeAOTokenLocked(sess session) {
	if sess.aoToken == "" {
		return
	}
	delete(a.aoTokens, sess.aoToken)
}

// sessionAOEnv returns the AO_* environment of a thread's live session. Empty
// when the thread has no session — the credential only exists while the process
// that holds it does.
func (a *App) sessionAOEnv(threadID string) map[string]string {
	sess, ok := a.sessionManager().get(threadID)
	if !ok || len(sess.aoEnv) == 0 {
		return nil
	}
	env := make(map[string]string, len(sess.aoEnv))
	for name, value := range sess.aoEnv {
		env[name] = value
	}
	return env
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
	if scope.IsPhase() {
		env[aocli.EnvRunID] = scope.ItemID
		env[aocli.EnvPhaseID] = scope.PhaseID
	}
	return aoSessionCredential{token: token, scope: scope, env: env}, nil
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

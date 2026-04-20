package main

import (
	"context"
	"fmt"
	"log"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/store"

	"github.com/google/uuid"
)

// --- Session bindings and spawn helpers ---
//
// These methods own the provider-session lifecycle seen by Wails
// bindings. Start/stop plumbing that coordinates multiple in-flight
// start attempts lives in app_session_start.go; thin reconnect/switch
// wrappers live in app_session_bindings.go. Everything in this file
// runs the actual spawn and shutdown of a provider subprocess for a
// given thread.

// StartSession is the Wails-bound entry point for "bring this thread's
// provider subprocess up." The sendMessage path also calls
// startSessionNow via runSessionStart when a thread has no active
// session yet.
func (a *App) StartSession(threadID string) error {
	if a.shuttingDown.Load() {
		return ErrShuttingDown
	}
	return a.runSessionStart(threadID, func() error {
		return a.startSessionNow(threadID)
	})
}

// startSessionNow builds the provider-specific launch config, stops
// any prior session on the thread, and spawns a fresh one. Callers go
// through StartSession / runSessionStart so concurrent start attempts
// share a single spawn instead of racing.
func (a *App) startSessionNow(threadID string) error {
	t, err := a.store.GetThread(threadID)
	if err != nil {
		return fmt.Errorf("start session: %w", err)
	}

	sessionToken := uuid.NewString()
	onEvent := a.sessionEventHandler(threadID, sessionToken)

	// Design-mode plumbing (extra system prompt + Codex MCP servers) is
	// caller-owned: the provider package intentionally doesn't know about
	// design or discussion. We compose the final system prompt here, then
	// hand a provider-agnostic SessionOptions bundle to each provider's
	// ConfigFromOptions translator.
	designCfg, err := a.designSessionConfig(t)
	if err != nil {
		return fmt.Errorf("start session: %w", err)
	}
	systemPrompt := joinSystemPrompts(designCfg.Prompt, a.threadSystemPrompt(threadID))

	// Pending-fork intent is a one-shot. SessionOptionsFromThread reads
	// either PendingForkRef or SessionRef into opts.Resume based on this
	// flag. We consume it here (bool in scope) rather than mutating the
	// Thread row; any persistence changes belong to the caller that set
	// PendingForkRef originally.
	forkSession := t.PendingForkRef != ""

	opts := provider.SessionOptionsFromThread(t, systemPrompt, forkSession)

	if err := a.stopExistingSessionLocked(threadID); err != nil {
		return fmt.Errorf("start session: %w", err)
	}

	newSess, err := a.spawnProviderSession(threadID, sessionToken, t, opts, designCfg, onEvent)
	if err != nil {
		a.teardownDesignThread(threadID)
		a.emitProviderStatusOnSessionStartError(t.Provider)
		return fmt.Errorf("start session: %w", err)
	}

	a.mu.Lock()
	a.sessions[threadID] = newSess
	a.mu.Unlock()

	return nil
}

// stopExistingSessionLocked tears down any prior session for the thread
// before we start a replacement. Separated from startSessionNow so the
// caller reads top-down as "stop, compute options, spawn, register".
func (a *App) stopExistingSessionLocked(threadID string) error {
	a.mu.Lock()
	existing, ok := a.sessions[threadID]
	if ok {
		delete(a.sessions, threadID)
	}
	a.mu.Unlock()

	if !ok {
		return nil
	}

	// Thread-scoped design state must be torn down alongside the session
	// so a restart doesn't leak the prior turn's MCP registration.
	a.teardownDesignThread(threadID)
	return closeProviderSession(threadID, existing)
}

// spawnProviderSession builds the provider-specific Config via
// ConfigFromOptions + per-provider ancillary wiring (binary path, MCP
// servers for Codex, event logger) and calls the provider's NewSession
// constructor. Returns a populated session wrapper ready to register in
// a.sessions.
func (a *App) spawnProviderSession(
	threadID, sessionToken string,
	t store.Thread,
	opts provider.SessionOptions,
	designCfg designSessionConfig,
	onEvent func(provider.ProviderEvent),
) (session, error) {
	switch t.Provider {
	case string(provider.Claude):
		cfg := claude.ConfigFromOptions(opts)
		cfg.Binary = a.providerBinaryPath(t.Provider)
		cfg.EventLogger = a.logger
		sess, err := claude.NewSession(context.Background(), threadID, cfg, onEvent)
		if err != nil {
			return session{}, err
		}
		return session{
			provider: string(provider.Claude),
			token:    sessionToken,
			claude:   sess,
		}, nil

	case string(provider.Codex):
		cfg := codex.ConfigFromOptions(opts)
		cfg.Binary = a.providerBinaryPath(t.Provider)
		cfg.EventLogger = a.logger
		cfg.MCPServers = designCfg.MCPServers
		sess, err := codex.NewSession(context.Background(), threadID, cfg, onEvent)
		if err != nil {
			return session{}, err
		}
		return session{
			provider: string(provider.Codex),
			token:    sessionToken,
			codex:    sess,
		}, nil

	default:
		return session{}, fmt.Errorf("unknown provider: %s", t.Provider)
	}
}

// SendMessage is the Wails-bound entry point for user-typed content.
// The real work lives in app_send.go.
func (a *App) SendMessage(threadID string, content string) error {
	if a.shuttingDown.Load() {
		return ErrShuttingDown
	}
	return a.sendMessage(threadID, content)
}

// InterruptTurn fires a provider-level interrupt on the thread's active
// session. Returns an error when no session is active or the provider
// surface isn't wired up.
//
// Spec: on user interrupt, any streaming items on the current turn are
// flipped to errored with a " — stopped" suffix, and a system `error`
// row with Summary "Stopped by user" is appended. This happens AFTER
// the provider interrupt signal is sent so the UI gets a consistent
// "signal sent, now here's the record" ordering. If the triage
// bookkeeping fails we log — the provider interrupt already fired, so
// the session state is correct even if the timeline marker is missing.
func (a *App) InterruptTurn(threadID string) error {
	a.mu.Lock()
	sess, ok := a.sessions[threadID]
	a.mu.Unlock()
	if !ok {
		return fmt.Errorf("no active session for thread %s", threadID)
	}

	providerSess := sess.providerSession()
	if providerSess == nil {
		return fmt.Errorf("session has no provider")
	}
	if err := providerSess.Interrupt(context.Background()); err != nil {
		return err
	}
	if a.triage != nil {
		if _, err := a.triage.MarkUserInterrupt(threadID); err != nil {
			log.Printf("interrupt turn: mark user interrupt: %v", err)
		}
	}
	return nil
}

// StopSession tears down the thread's provider session. Idempotent: a
// thread with no active session still runs the design teardown +
// triage cleanup so stale per-thread state doesn't leak.
func (a *App) StopSession(threadID string) error {
	a.mu.Lock()
	sess, ok := a.sessions[threadID]
	if ok {
		delete(a.sessions, threadID)
	}
	a.mu.Unlock()

	if !ok {
		a.teardownDesignThread(threadID)
		if a.triage != nil {
			a.triage.CleanupThread(threadID)
		}
		return nil
	}

	a.teardownDesignThread(threadID)
	if a.triage != nil {
		a.triage.CleanupThread(threadID)
	}
	return closeProviderSession(threadID, sess)
}

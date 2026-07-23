package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/providerstatus"
)

// rateLimitProbeHTTPClient is the shared HTTP client used by every
// rate-limit probe. Singleton at the app level so the underlying
// transport's connection-keepalive pool spans the probe cadence;
// timeouts are enforced per-request via context inside
// claude.ProbeRateLimits.
var rateLimitProbeHTTPClient = &http.Client{}

// probeClaudeRateLimits runs one rate-limit probe and emits the
// snapshot on the `provider:usage` channel. Rate-limit data is
// account-wide so the event carries no threadId — the frontend
// rate-limits store keys by provider only.
//
// Returns silently on ErrNoCredentials — the user simply hasn't run
// `claude login` yet and there's nothing useful to do until they do.
// Other errors are logged at the standard log level: rate-limit data
// is non-critical (the rings just stay at their last-known value), so
// transient failures should not surface a banner.
func (a *App) probeClaudeRateLimits(ctx context.Context) {
	// Cheap fail-fast: ctx cancellation would also short-circuit the
	// HTTP call below, but skipping the credential read and HTTP setup
	// is cheaper than letting them allocate and immediately abort.
	if a.shuttingDown.Load() {
		return
	}
	a.providerAccountMu.RLock()
	selection := a.providerAccountSelectionLocked(string(provider.Claude))
	var (
		snap provider.RateLimitsSnapshot
		err  error
	)
	if selection.CredentialPath != "" {
		snap, err = a.probeClaudeRateLimitsForSelection(ctx, selection)
	} else {
		snap, err = claude.ProbeRateLimits(ctx, a.rateLimitProbeClient())
	}
	if err == nil {
		snap.AccountID = selection.AccountID
	}
	a.providerAccountMu.RUnlock()
	if err != nil {
		if errors.Is(err, claude.ErrNoCredentials) {
			return
		}
		log.Printf("claude: rate-limit probe: %v", err)
		return
	}
	a.emitRateLimitsSnapshot(snap)
}

func (a *App) probeClaudeRateLimitsForSelection(
	ctx context.Context,
	selection providerAccountSelection,
) (provider.RateLimitsSnapshot, error) {
	var snapshot provider.RateLimitsSnapshot
	var err error
	if a.providerCredentials != nil && selection.AccountID != "" {
		var data []byte
		data, err = a.providerCredentials.ReadCredential(
			string(provider.Claude),
			selection.AccountID,
			selection.CredentialActive,
		)
		if err == nil {
			snapshot, err = claude.ProbeRateLimitsFromCredentialData(
				ctx,
				a.rateLimitProbeClient(),
				data,
			)
		}
	} else {
		snapshot, err = claude.ProbeRateLimitsFromCredentialPath(
			ctx,
			a.rateLimitProbeClient(),
			selection.CredentialPath,
		)
	}
	if !errors.Is(err, claude.ErrOAuthUnauthorized) || selection.Home == "" {
		return snapshot, err
	}

	// Let Claude own refresh-token rotation exactly as it does before a
	// normal turn. The zero-turn account probe initializes the CLI without
	// inference and writes refreshed credentials back to the selected native
	// home. Retry the usage endpoint only after that native path completes.
	info, refreshErr := claude.ProbeAccount(ctx, claude.ProbeConfig{
		Binary: a.providerBinaryPath(string(provider.Claude)),
		Env:    selection.Env,
	})
	if refreshErr != nil {
		return provider.RateLimitsSnapshot{}, fmt.Errorf("refresh Claude credentials: %w", refreshErr)
	}
	if providerstatus.ClaudeUnauthenticated(info) {
		return provider.RateLimitsSnapshot{}, errors.New("Claude credentials expired; log in again")
	}
	if a.providerCredentials != nil && selection.AccountID != "" {
		data, readErr := a.providerCredentials.ReadCredential(
			string(provider.Claude),
			selection.AccountID,
			selection.CredentialActive,
		)
		if readErr != nil {
			return provider.RateLimitsSnapshot{}, readErr
		}
		return claude.ProbeRateLimitsFromCredentialData(ctx, a.rateLimitProbeClient(), data)
	}
	return claude.ProbeRateLimitsFromCredentialPath(ctx, a.rateLimitProbeClient(), selection.CredentialPath)
}

// rateLimitProbeClient returns the HTTP client used by the probe.
// Test-only override on the App struct takes precedence so tests can
// inject a fake server. Production callers see the default package
// client, which has no transport overrides.
func (a *App) rateLimitProbeClient() *http.Client {
	if a.rateLimitProbeClientOverride != nil {
		return a.rateLimitProbeClientOverride
	}
	return rateLimitProbeHTTPClient
}

// startClaudeRateLimitProbeLoop fires an initial probe at startup,
// then re-probes every `rateLimitProbeInterval` while at least
// one Claude session is alive.
//
// Three trigger points feed into the same probe:
//  1. Startup (this function) — once at app boot.
//  2. Periodic — every 2 mins while sessions exist.
//  3. Turn complete — fired from sessionEventHandler for Claude
//     sessions so an active user sees the rings refresh after each
//     model response.
//
// Stop semantics: the loop's `select` arms on `appCtx.Done()` so
// Shutdown step 1b's `appCancel()` breaks it out immediately, and the
// in-flight HTTP probe receives the same cancellation through the
// lifeCtx-derived ctx and aborts mid-request. The HTTP client has no
// long-lived resources to leak.
func (a *App) startClaudeRateLimitProbeLoop() {
	a.startRateLimitProbeLoop(rateLimitProbeLoop{
		// Startup account probing invokes the first usage read after adopting
		// an existing native login, so the snapshot is account-scoped.
		probeImmediately: false,
		hasActiveSession: a.hasActiveClaudeSession,
		probe:            a.probeClaudeRateLimits,
	})
}

// hasActiveClaudeSession reports whether at least one Claude session
// is registered. The periodic probe loop gates on this so an idle app
// doesn't burn probes against the Messages API. Snapshot under the
// session lock so a concurrent start/stop can't race the iteration.
func (a *App) hasActiveClaudeSession() bool {
	return a.sessionManager().hasProvider(string(provider.Claude))
}

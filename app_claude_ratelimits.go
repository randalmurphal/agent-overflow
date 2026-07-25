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
// snapshot on the `provider:usage` channel. Rate-limit data is account-wide
// so the event carries no threadId — the frontend store keys it by provider,
// account, limit ID, and duration.
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
	if err := a.reconcileExternalProviderAccount(string(provider.Claude)); err != nil {
		log.Printf("claude: reconcile external account before rate-limit probe: %v", err)
		return
	}
	selection := a.captureProviderAccountSelection(string(provider.Claude))
	if selection.AccountID != "" {
		if err := a.refreshProviderAccountUsage(
			ctx,
			string(provider.Claude),
			selection.AccountID,
		); err != nil {
			if errors.Is(err, claude.ErrNoCredentials) {
				return
			}
			log.Printf("claude: rate-limit probe: %v", err)
		}
		return
	}
	var (
		snap provider.RateLimitsSnapshot
		err  error
	)
	snap, err = claude.ProbeRateLimits(ctx, a.rateLimitProbeClient())
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
	selection claudeRateLimitSelection,
) (provider.RateLimitsSnapshot, []byte, error) {
	if a.providerCredentials == nil || selection.EphemeralHome == nil {
		return provider.RateLimitsSnapshot{}, nil, errors.New(
			"Claude rate-limit probe requires temporary credentials",
		)
	}
	data, err := a.readClaudeSelectionCredential(selection)
	if err != nil {
		return provider.RateLimitsSnapshot{}, nil, err
	}
	snapshot, err := claude.ProbeRateLimitsFromCredentialData(
		ctx,
		a.rateLimitProbeClient(),
		data,
	)
	if !errors.Is(err, claude.ErrOAuthUnauthorized) {
		return snapshot, nil, err
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
		return provider.RateLimitsSnapshot{}, nil, fmt.Errorf("refresh Claude credentials: %w", refreshErr)
	}
	if providerstatus.ClaudeUnauthenticated(info) {
		return provider.RateLimitsSnapshot{}, nil, errors.New("Claude credentials expired; log in again")
	}
	data, readErr := a.readClaudeSelectionCredential(selection)
	if readErr != nil {
		return provider.RateLimitsSnapshot{}, nil, readErr
	}
	snapshot, retryErr := claude.ProbeRateLimitsFromCredentialData(
		ctx,
		a.rateLimitProbeClient(),
		data,
	)
	return snapshot, data, retryErr
}

func (a *App) readClaudeSelectionCredential(
	selection claudeRateLimitSelection,
) ([]byte, error) {
	snapshot, err := a.providerCredentials.ReadEphemeralCredential(selection.EphemeralHome)
	return snapshot.Data, err
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

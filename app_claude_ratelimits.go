package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

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
// Every automatic trigger reaches this through claudeUsageGate(), never
// directly — the gate coalesces bursts and enforces the cooldown. A server
// 429 is scoped per account by usageBackoffLedger inside the refresh path,
// which refuses further requests for that account until the backoff expires.
//
// Returns nil on ErrNoCredentials — the user simply hasn't run
// `claude login` yet and there's nothing useful to do until they do.
// Other errors are also logged here: rate-limit data is non-critical
// (the rings just stay at their last-known value), so transient
// failures should not surface a banner.
func (a *App) probeClaudeRateLimits(ctx context.Context) error {
	// Cheap fail-fast: ctx cancellation would also short-circuit the
	// HTTP call below, but skipping the credential read and HTTP setup
	// is cheaper than letting them allocate and immediately abort.
	if a.shuttingDown.Load() {
		return nil
	}
	if err := a.reconcileExternalProviderAccount(string(provider.Claude)); err != nil {
		log.Printf("claude: reconcile external account before rate-limit probe: %v", err)
		return err
	}
	selection := a.captureProviderAccountSelection(string(provider.Claude))
	if selection.AccountID != "" {
		err := a.refreshProviderAccountUsage(
			ctx,
			string(provider.Claude),
			selection.AccountID,
		)
		if err != nil {
			if errors.Is(err, claude.ErrNoCredentials) {
				return nil
			}
			log.Printf("claude: rate-limit probe: %v", err)
		}
		return err
	}
	// No managed account yet: the canonical login is probed directly, keyed in
	// the backoff ledger under the empty account ID so a 429 here still holds
	// follow-up probes without sending anything.
	if remaining := a.usageBackoff.Remaining(string(provider.Claude), ""); remaining > 0 {
		return fmt.Errorf(
			"the usage endpoint rate limited this login; try again in %s",
			remaining.Round(time.Second),
		)
	}
	snap, err := claude.ProbeRateLimits(ctx, a.rateLimitProbeClient())
	a.usageBackoff.Note(string(provider.Claude), "", err)
	if err != nil {
		if errors.Is(err, claude.ErrNoCredentials) {
			return nil
		}
		log.Printf("claude: rate-limit probe: %v", err)
		return err
	}
	a.emitRateLimitsSnapshot(snap)
	return nil
}

// probeSelectedClaudeRateLimits probes the account Agent Overflow currently
// has selected, refreshing in the canonical home when the token has expired.
//
// The selected account's credential IS the canonical one, so its refresh must
// happen there. Claude serializes refresh-token rotation on a lockfile scoped
// to the config home and its refresh tokens are single-use: rotating a copy in
// a temporary home takes a different lock, and the winner's rotation retires
// the token the canonical home still holds. That is a login the user cannot
// recover without signing in again — the exact failure this whole path exists
// to avoid. An inactive account has no canonical copy to fork, which is why
// probeClaudeRateLimitsForSelection can keep using a temporary home.
//
// Returns the canonical credential as it stands after a refresh, or nil when
// no refresh was needed.
func (a *App) probeSelectedClaudeRateLimits(
	ctx context.Context,
	selection providerAccountSelection,
	credential []byte,
) (provider.RateLimitsSnapshot, []byte, error) {
	snapshot, err := claude.ProbeRateLimitsFromCredentialData(
		ctx,
		a.rateLimitProbeClient(),
		credential,
	)
	if !errors.Is(err, claude.ErrOAuthUnauthorized) {
		return snapshot, nil, err
	}

	// A zero-turn probe against the canonical home is Claude's own refresh
	// path, lock and all. It reports who it authenticated as, which is a
	// stronger check than comparing bytes: a rotation legitimately changes
	// the credential, but the account behind it must not change.
	//
	// CLAUDE_CONFIG_DIR is deliberately left unset (ProbeAccount clears any
	// inherited value) rather than pointed at the canonical home. Claude
	// treats the variable's presence — not its value — as "non-default
	// home", and on macOS a non-default home hashes into a different
	// Keychain service. Setting it to the default path would send the
	// rotated credential somewhere Agent Overflow never reads.
	info, refreshErr := claude.ProbeAccount(ctx, a.claudeProbeConfig(
		a.providerBinaryPath(string(provider.Claude)),
		nil,
	))
	if refreshErr != nil {
		return provider.RateLimitsSnapshot{}, nil, fmt.Errorf("refresh Claude credentials: %w", refreshErr)
	}
	if providerstatus.ClaudeUnauthenticated(info) {
		return provider.RateLimitsSnapshot{}, nil, errors.New("Claude credentials expired; log in again")
	}
	if err := a.assertSelectedClaudeIdentity(selection, info); err != nil {
		return provider.RateLimitsSnapshot{}, nil, err
	}
	refreshed, readErr := a.providerCredentials.ReadCredentialSnapshot(
		string(provider.Claude),
		"",
		true,
	)
	if readErr != nil {
		return provider.RateLimitsSnapshot{}, nil, fmt.Errorf(
			"read refreshed Claude credentials: %w",
			readErr,
		)
	}
	snapshot, retryErr := claude.ProbeRateLimitsFromCredentialData(
		ctx,
		a.rateLimitProbeClient(),
		refreshed.Data,
	)
	return snapshot, refreshed.Data, retryErr
}

// assertSelectedClaudeIdentity refuses to attribute a canonical-home refresh
// to the selected account when the CLI authenticated as someone else — an
// external `claude login` during the probe. An empty email cannot contradict
// anything: Claude reports one only once it has re-derived the identity for
// the installed credential.
func (a *App) assertSelectedClaudeIdentity(
	selection providerAccountSelection,
	info provider.AccountInfo,
) error {
	observed := strings.TrimSpace(info.Email)
	expected := strings.TrimSpace(selection.Account.Email)
	if observed == "" || expected == "" || strings.EqualFold(observed, expected) {
		return nil
	}
	return fmt.Errorf(
		"the active Claude account changed to %s while refreshing usage; retry",
		observed,
	)
}

// probeClaudeRateLimitsForSelection probes an account that is NOT selected,
// from a temporary home seeded with only its credential. That account has no
// canonical copy, so it is the only holder of its refresh chain and a
// temporary home cannot fork anything — see probeSelectedClaudeRateLimits for
// why the selected account must not take this path.
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
	// inference and writes refreshed credentials back to the temporary home.
	// Retry the usage endpoint only after that native path completes.
	info, refreshErr := claude.ProbeAccount(ctx, a.claudeProbeConfig(
		a.providerBinaryPath(string(provider.Claude)),
		selection.Env,
	))
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
		probe:            a.claudeUsageGate().Request,
	})
}

// hasActiveClaudeSession reports whether at least one Claude session
// is registered. The periodic probe loop gates on this so an idle app
// doesn't burn probes against the Messages API. Snapshot under the
// session lock so a concurrent start/stop can't race the iteration.
func (a *App) hasActiveClaudeSession() bool {
	return a.sessionManager().hasProvider(string(provider.Claude))
}

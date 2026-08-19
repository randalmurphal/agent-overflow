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

// errClaudeUsageStale reports a usage refresh that was declined rather than
// attempted, because the account's own OAuth session is over. It is a sentinel
// so callers can tell it apart from a failed request: nothing was sent, so
// there is no throttle to record and no credential to commit.
//
// Its text is what the user reads on the account card.
var errClaudeUsageStale = errors.New(
	"usage was not refreshed because this account's sign-in has expired; " +
		"the last known usage is shown until you switch to this account, " +
		"which signs it back in on its next use",
)

// errClaudeCredentialSignedOut reports credential bytes that are the CLI's
// sign-out husk: the provider gave up on this token chain, and only a fresh
// login replaces it. Callers with account metadata map it onto
// errProviderAccountNeedsLogin so the user is told which account to repair.
var errClaudeCredentialSignedOut = errors.New("the Claude CLI signed this login out; sign in again")

// errClaudeCredentialsExpired reports a credential that survived a canonical
// refresh — it is not the sign-out husk, and the CLI kept it — yet the server
// still refuses the bearer it holds. Only a fresh login replaces it.
//
// It is raised from the usage retry and nowhere else, because that request is
// the only thing on this path that asks the server about the bytes we actually
// hold. Deriving it from what the probe REPORTED instead is what made a
// perfectly good account read as dead every time it was switched to.
var errClaudeCredentialsExpired = errors.New("Claude credentials expired; log in again")

// probeSelectedClaudeRateLimits probes the account Agent Overflow currently
// has selected, refreshing in the canonical home when the token has expired.
//
// This is the ONE path in the app that may rotate a Claude refresh token. The
// selected account's credential IS the canonical one, so its refresh must
// happen there. Claude serializes refresh-token rotation on a lockfile scoped
// to the config home and its refresh tokens are single-use: rotating a copy in
// a temporary home takes a different lock, and the winner's rotation retires
// the token the canonical home still holds. That is a login the user cannot
// recover without signing in again — the exact failure this whole path exists
// to avoid. Inactive accounts do not refresh at all; see
// probeInactiveClaudeRateLimits.
//
// Returns the canonical credential as it stands after a refresh, or nil when
// no refresh was needed.
func (a *App) probeSelectedClaudeRateLimits(
	ctx context.Context,
	selection providerAccountSelection,
	credential []byte,
) (provider.RateLimitsSnapshot, []byte, error) {
	// A canonical credential that is already the husk means the CLI ended this
	// login before we got here (a failed refresh during a session, or a boot
	// that inherited the brick). Without this check the HTTP probe reports the
	// blank token as ErrNoCredentials — "run claude login" — instead of naming
	// the account that needs repair, and nothing reaches the audit log.
	if claude.CredentialsSignedOut(credential) {
		a.auditAccountEvent(
			"claude account %s found signed out: the canonical credential is the CLI's sign-out marker",
			selection.AccountID,
		)
		return provider.RateLimitsSnapshot{}, nil, errClaudeCredentialSignedOut
	}
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
	refreshCfg := a.claudeProbeConfig(a.providerBinaryPath(string(provider.Claude)), nil)
	// The server just rejected this bearer, which is what brought us here.
	// That makes the CLI force a refresh regardless of the stored expiry, so
	// the probe must hold its process for the rotation even when the bytes on
	// disk still look fresh.
	refreshCfg.RotationExpected = true
	info, refreshErr := claude.ProbeAccount(ctx, refreshCfg)
	if refreshErr != nil {
		return provider.RateLimitsSnapshot{}, nil, fmt.Errorf("refresh Claude credentials: %w", refreshErr)
	}
	// Ordering dependency: this read is only correct because ProbeAccount does
	// not return until an expected rotation has actually landed (see
	// claude.ProbeConfig.ReadCredential). Without that, the read races the
	// CLI's own credential write — and losing that race is not a stale read,
	// it is a brick: these bytes go straight into CommitSelectedCredential,
	// which would overwrite the CLI's freshly rotated pair with the retired
	// one it just replaced.
	//
	// The bytes are the verdict, not the probe's account object — so read them
	// BEFORE weighing what the probe reported. A refresh the server answered
	// with invalid_grant leaves the CLI blanking the credential in place while
	// its probe still answers success, echoing the husk's residual fields
	// (subscriptionType always, email when the canonical home's oauthAccount
	// record survives). Whatever shape that echo takes, the blank tokens
	// underneath must reach neither the retry below nor the caller's commit.
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
	if claude.CredentialsSignedOut(refreshed.Data) {
		a.auditAccountEvent(
			"claude account %s found signed out: the CLI blanked the canonical credential during a usage refresh",
			selection.AccountID,
		)
		return provider.RateLimitsSnapshot{}, nil, errClaudeCredentialSignedOut
	}
	// There is deliberately NO ClaudeUnauthenticated check here, and adding one
	// back is a credential-destroying change.
	//
	// That helper asks whether the probe reported any identity. On this path it
	// cannot: the CLI reads its identity from `~/.claude.json`'s `oauthAccount`
	// record, AO CLEARS that record on every account switch
	// (retireProviderIdentity, by design — the provider must re-derive it
	// rather than describe one account's email over another's tokens), and the
	// CLI only writes it back from a profile fetch it performs asynchronously
	// during the very startup this probe drives. So the FIRST canonical refresh
	// after any switch sees an empty account object no matter how healthy the
	// login is, and reading that as "expired" both lied to the user and — via
	// the nil credential below — threw away the rotation the probe had just
	// spent. Observed 2026-08-18 19:37:30: a switch at 19:37:26, a successful
	// rotation at 19:37:29, and "Claude credentials expired; log in again" one
	// second later with the fresh pair left uncommitted.
	//
	// The usable verdict is the bytes: the husk check above, then the retry
	// below, which asks the server about the credential we actually hold.
	if err := a.assertSelectedClaudeIdentity(selection, info); err != nil {
		// The one case whose rotation is not ours to keep: an external
		// `claude login` landed a DIFFERENT account's pair in the canonical
		// home. Committing it into this account's slot would pair one login's
		// tokens with another's identity, so it stays where it is.
		return provider.RateLimitsSnapshot{}, nil, err
	}
	snapshot, retryErr := claude.ProbeRateLimitsFromCredentialData(
		ctx,
		a.rateLimitProbeClient(),
		refreshed.Data,
	)
	if errors.Is(retryErr, claude.ErrOAuthUnauthorized) {
		// A bearer the server rejects immediately after its own refresh issued
		// it is a chain that is genuinely over — the byte-derived form of the
		// verdict the account object used to guess at.
		retryErr = errClaudeCredentialsExpired
	}
	// refreshed.Data goes back on EVERY path from here, error included. Claude
	// refresh tokens are single-use, so a rotation that reaches disk but not
	// the commit leaves this account's slot holding a token the server has
	// already retired — a login that dies at its next use, not a stale read.
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

// probeInactiveClaudeRateLimits reads usage for an account that is NOT
// selected, using its saved credential and nothing else. It never spawns the
// CLI, never rotates a token, and never returns credential bytes to commit.
//
// Refreshing an inactive account was the destructive path. Anthropic's token
// endpoint commits a refresh-token rotation the moment it processes the
// request — before the client sees the response, with no grace window on the
// retired token — so a dropped connection or a killed CLI during a refresh AO
// asked for on the user's behalf ends that account's chain for good. The next
// attempt gets invalid_grant, the CLI blanks the credential in place, and its
// probe still answers success with the husk's residual plan label. Nothing
// downstream can undo that, so the refresh does not happen: an expired
// inactive account reports stale usage and waits for the user to select it,
// where the CLI owns the rotation in the canonical home as it always has.
func (a *App) probeInactiveClaudeRateLimits(
	ctx context.Context,
	credential []byte,
) (provider.RateLimitsSnapshot, error) {
	if claude.CredentialsSignedOut(credential) {
		return provider.RateLimitsSnapshot{}, errClaudeCredentialSignedOut
	}
	// Checked before the request, not after: an expired bearer earns a 401
	// that says nothing, and the usage endpoint's 429 throttle is per-bearer
	// and shared across every machine on the account. Spending one of those
	// on a token we can already read as dead is pure cost.
	if claude.CredentialExpired(credential, time.Now()) {
		return provider.RateLimitsSnapshot{}, errClaudeUsageStale
	}
	snapshot, err := claude.ProbeRateLimitsFromCredentialData(
		ctx,
		a.rateLimitProbeClient(),
		credential,
	)
	if errors.Is(err, claude.ErrOAuthUnauthorized) {
		// The server disagrees with the stored expiry — same outcome, and
		// still not ours to heal.
		return provider.RateLimitsSnapshot{}, errClaudeUsageStale
	}
	return snapshot, err
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

// startClaudeRateLimitProbeLoop starts the ONE automatic usage poll:
// every `rateLimitProbeInterval`, and only when a Claude turn completed
// since the previous poll (sessionEventHandler records the activity
// mark). An idle app — open threads included, since Claude processes
// stay alive between turns — sends nothing. The startup account probe
// (app_account_probe.go) covers the boot-time read.
//
// The usage endpoint's 429 throttle is per-bearer and therefore SHARED
// by every machine logged into the same account; this loop's cadence is
// each machine's whole automatic budget, so any new automatic trigger
// must route through this loop's activity mark, never probe directly.
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
		turnCompletedSince: func(mark time.Time) bool {
			return a.providerTurnCompletedSince(string(provider.Claude), mark)
		},
		probe: a.claudeUsageGate().Request,
	})
}

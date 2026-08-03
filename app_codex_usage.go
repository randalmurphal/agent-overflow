package main

import (
	"context"
	"errors"
	"log"
	"strings"

	"agent-overflow/internal/codexusage"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/codex"
)

// CodexAccountUsage is Codex's own token-usage report for the signed-in
// account, as the usage overlay renders it.
//
// It is NOT the same population as AO's usage ledger, and the UI says so:
// this counts every turn the account ran anywhere (the Codex TUI, another
// editor, another machine), while the ledger counts what AO itself observed
// and prices from a local rate table. Showing them side by side is the whole
// point — one is the provider's ground truth for tokens, the other is AO's
// estimate for cost.
//
// Every summary field is a pointer because the backend genuinely omits values
// it has no history for. Absence renders as absence; it never becomes zero.
type CodexAccountUsage struct {
	LifetimeTokens        *int64                    `json:"lifetimeTokens,omitempty"`
	PeakDailyTokens       *int64                    `json:"peakDailyTokens,omitempty"`
	LongestRunningTurnSec *int64                    `json:"longestRunningTurnSec,omitempty"`
	CurrentStreakDays     *int64                    `json:"currentStreakDays,omitempty"`
	LongestStreakDays     *int64                    `json:"longestStreakDays,omitempty"`
	DailyBuckets          []CodexAccountUsageBucket `json:"dailyBuckets"`
	// AccountEmail identifies whose report this is, so the section can never
	// look like it describes the account the user just switched away from.
	// Empty when AO holds no metadata for the active login.
	AccountEmail string `json:"accountEmail,omitempty"`
}

// CodexAccountUsageBucket is one day of account-wide token usage. StartDate
// is Codex's own date string; days with no usage are absent rather than zero.
type CodexAccountUsageBucket struct {
	StartDate string `json:"startDate"`
	Tokens    int64  `json:"tokens"`
}

// GetCodexAccountUsage returns Codex's account-level usage report, or nil
// when there is nothing to report.
//
// The nil-with-nil-error result is a state answer, not a swallowed failure,
// and it covers exactly three cases, all of which mean "do not render this
// section" rather than "render zeros":
//
//   - the installed codex predates `account/usage/read` (it landed in 0.138.0,
//     below AO's 0.143 provider floor, so this is only reachable when the
//     floor itself is bypassed);
//   - the signed-in account is not a ChatGPT account, so it has no usage
//     profile at all (an API-key login);
//   - the backend answered with an empty profile (a brand-new account).
//
// Anything else — a spawn failure, a timeout, a malformed response — is
// returned as an error so it is visible rather than mistaken for absence.
//
// Local-only on the wire: it spawns the Codex CLI (or drives a live session)
// under the user's credentials and returns account-scoped data.
func (a *App) GetCodexAccountUsage() (*CodexAccountUsage, error) {
	selection := a.captureProviderAccountSelection(string(provider.Codex))
	binary := a.providerBinaryPath(string(provider.Codex))
	key := binary + "\x00" + selection.AccountID

	usage, err := a.codexAccountUsage().Get(a.lifeCtx(), key, func(ctx context.Context) (codex.AccountUsage, error) {
		return a.readCodexAccountUsage(ctx, binary, selection.AccountID)
	})
	if err != nil {
		if errors.Is(err, codex.ErrAccountUsageUnavailable) {
			// Logged, not returned: the overlay's answer is "no section", and
			// a cached-unavailable entry bounds this to once per error TTL
			// rather than once per render.
			log.Printf("codex account usage unavailable: %v", err)
			return nil, nil
		}
		return nil, err
	}
	if usage.Empty() {
		return nil, nil
	}

	projected := &CodexAccountUsage{
		LifetimeTokens:        usage.LifetimeTokens,
		PeakDailyTokens:       usage.PeakDailyTokens,
		LongestRunningTurnSec: usage.LongestRunningTurnSec,
		CurrentStreakDays:     usage.CurrentStreakDays,
		LongestStreakDays:     usage.LongestStreakDays,
		DailyBuckets:          make([]CodexAccountUsageBucket, 0, len(usage.DailyBuckets)),
		AccountEmail:          strings.TrimSpace(selection.Account.Email),
	}
	for _, bucket := range usage.DailyBuckets {
		projected.DailyBuckets = append(projected.DailyBuckets, CodexAccountUsageBucket{
			StartDate: bucket.StartDate,
			Tokens:    bucket.Tokens,
		})
	}
	return projected, nil
}

// readCodexAccountUsage prefers a live Codex session's already-open
// app-server connection and falls back to a short-lived process.
//
// Riding a live session is not just an optimization: `account/usage/read` is
// global (`serialization: None`) and touches no thread state, so a session
// answers it for one JSON-RPC round trip instead of a cold binary start plus
// a second handshake. The session is only trusted when its process
// authenticated as the account AO currently considers active — a session
// still running under a superseded credential would report the previous
// login's lifetime totals under the new account's name.
func (a *App) readCodexAccountUsage(ctx context.Context, binary, accountID string) (codex.AccountUsage, error) {
	if sess, sessionAccountID := a.sessionManager().anyCodexSession(); sess != nil && sessionAccountID == accountID {
		usage, err := sess.ReadAccountUsage(ctx)
		if err == nil {
			return usage, nil
		}
		if errors.Is(err, codex.ErrAccountUsageUnavailable) {
			return codex.AccountUsage{}, err
		}
		// A live session can fail for reasons the account cannot (the process
		// died mid-read, the request timed out). Fall through to a fresh
		// process rather than reporting a transport problem as an account
		// answer.
		log.Printf("codex account usage: live session read failed, falling back to a fresh process: %v", err)
	}
	if strings.TrimSpace(binary) == "" {
		return codex.AccountUsage{}, errors.New("codex account usage: codex binary not configured")
	}
	fetcher := &codex.AccountUsageFetcher{
		Binary:  binary,
		WorkDir: providerProbeWorkDir(),
		Env:     a.providerProbeEnv(string(provider.Codex), nil),
	}
	return fetcher.Fetch(ctx)
}

func (a *App) codexAccountUsage() *codexusage.Cache {
	a.codexAccountUsageOnce.Do(func() {
		a.codexAccountUsageCache = codexusage.New()
	})
	return a.codexAccountUsageCache
}

package app

import (
	"context"
	"strings"

	"agent-overflow/internal/codexapp"
	"agent-overflow/internal/codexskills"
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

// CleanCodexBackgroundTerminals asks the Codex app-server to terminate
// every running unified-exec background PTY for `threadID`. This is the
// thread-wide "Stop all" primitive for Codex; the per-row stop is
// TerminateCodexBackgroundTerminal below.
//
// After the RPC succeeds, Codex emits one `item/completed` notification
// per terminated PTY. Those update triage's transient tray state; the
// command output becomes transcript history only if the model explicitly
// waits/polls the terminal with write_stdin. No follow-up work is needed
// here.
//
// Returns typed errors for:
//
//   - session-missing: no Codex session for this thread. The caller
//     reached the binding before Start / after Close.
//   - provider-mismatch: the thread exists but it's a Claude session.
//     Claude has its own per-row stop primitive (StopClaudeTask); the
//     frontend must branch on provider before reaching for this.
//   - timeout / provider error: surfaced verbatim so the UI can render
//     the CLI-supplied message.
func (a *App) CleanCodexBackgroundTerminals(threadID string) error {
	if a.shuttingDown.Load() {
		return ErrShuttingDown
	}
	return a.codexAppService().CleanBackgroundTerminals(threadID)
}

// TerminateCodexBackgroundTerminal stops ONE running unified-exec
// background PTY on `threadID`, identified by the app-server process id
// the tray row carries as `meta.process_id`. It is the Codex counterpart
// of StopClaudeTask — same tray affordance, different id namespace and a
// different RPC (`thread/backgroundTerminals/terminate`), which is why
// the frontend branches on provider rather than sharing one binding.
//
// The bool is the wire's own answer, not a success flag: `false, nil`
// means the RPC succeeded and matched no running process (the shell had
// already exited, or the id belongs to another thread). Callers must
// surface that as state — a stop that killed nothing emits no follow-up
// `item/completed`, so silently discarding it would leave the user
// staring at a row that never changes.
//
// On a real termination Codex emits `item/completed` for that PTY, which
// flows through the existing triage path and clears the tray row. No
// follow-up work is needed here.
//
// Returns typed errors for:
//
//   - session-missing: no Codex session for this thread. The caller
//     reached the binding before Start / after Close.
//   - provider-mismatch: the thread exists but it's a Claude session.
//     Claude's per-row stop is StopClaudeTask, keyed by task id.
//   - blank process id / timeout / provider error: surfaced verbatim so
//     the UI can render the CLI-supplied message.
func (a *App) TerminateCodexBackgroundTerminal(threadID, processID string) (bool, error) {
	if a.shuttingDown.Load() {
		return false, ErrShuttingDown
	}
	return a.codexAppService().TerminateBackgroundTerminal(threadID, processID)
}

// StopCodexSubagent interrupts the live child turn owned by launchID. The
// provider resolves the launch through its typed child-ownership map.
func (a *App) StopCodexSubagent(threadID, launchID string) (bool, error) {
	if a.shuttingDown.Load() {
		return false, ErrShuttingDown
	}
	return a.codexAppService().StopSubagent(threadID, launchID)
}

// GetCodexSkills returns the Codex skills visible from one workspace
// directory — the composer's command-menu source on a Codex thread, since
// skills are what upstream replaced custom prompts with in 0.118.
//
// workspacePath must be an ABSOLUTE path. Skills are directory-scoped (the
// repo tier comes from the workspace itself), and a relative path would be
// resolved against whichever process happens to answer — a live session's cwd
// or a throwaway one's — so it is refused rather than guessed at.
//
// forceReload is the user-initiated refresh and bypasses both AO's cache and
// the app-server's own on-disk scan. A menu opening must pass false, or every
// render re-walks the filesystem.
//
// LocalOnly on the wire: it drives the user's own `codex` CLI (a live
// session's connection when there is one, a short-lived app-server otherwise)
// and its answer names absolute paths on the host filesystem.
func (a *App) GetCodexSkills(ctx context.Context, workspacePath string, forceReload bool) (codexskills.CwdSkills, error) {
	return a.codexAppService().Skills(ctx, workspacePath, forceReload)
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
	result, err := a.codexAppService().AccountUsage()
	if err != nil || result == nil {
		return nil, err
	}
	usage := result.Usage
	projected := &CodexAccountUsage{
		LifetimeTokens:        usage.LifetimeTokens,
		PeakDailyTokens:       usage.PeakDailyTokens,
		LongestRunningTurnSec: usage.LongestRunningTurnSec,
		CurrentStreakDays:     usage.CurrentStreakDays,
		LongestStreakDays:     usage.LongestStreakDays,
		DailyBuckets:          make([]CodexAccountUsageBucket, 0, len(usage.DailyBuckets)),
		AccountEmail:          result.AccountEmail,
	}
	for _, bucket := range usage.DailyBuckets {
		projected.DailyBuckets = append(projected.DailyBuckets, CodexAccountUsageBucket{
			StartDate: bucket.StartDate,
			Tokens:    bucket.Tokens,
		})
	}
	return projected, nil
}

func (a *App) codexAppService() *codexapp.Service {
	a.codexAppOnce.Do(func() {
		a.codexApp = codexapp.New(codexapp.Deps{
			Session: func(threadID string) (*codex.Session, bool) {
				session, active := a.sessionManager().get(threadID)
				return session.Codex, active
			},
			SessionForBinary: a.sessionManager().codexSessionForBinary,
			AnySession:       a.sessionManager().anyCodexSession,
			Binary: func() string {
				return a.providerBinaryPath(string(provider.Codex))
			},
			CustomEnv: func() map[string]string {
				return a.providerCustomEnv(string(provider.Codex))
			},
			ProbeEnv: func() map[string]string {
				return a.providerProbeEnv(string(provider.Codex), nil)
			},
			ProbeWorkDir: providerProbeWorkDir,
			LifeContext:  a.lifeCtx,
			ActiveAccount: func() codexapp.AccountSelection {
				selection := a.captureProviderAccountSelection(string(provider.Codex))
				return codexapp.AccountSelection{
					ID: selection.AccountID, Email: strings.TrimSpace(selection.Account.Email),
				}
			},
		})
	})
	return a.codexApp
}

func (a *App) handleCodexSkillsChanged() {
	a.codexAppService().ResetSkills()
}

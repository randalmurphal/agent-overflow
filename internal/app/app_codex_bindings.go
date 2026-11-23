package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"

	"agent-overflow/internal/codexapp"
	"agent-overflow/internal/codexskills"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/triage"
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
// every running unified-exec background PTY of `threadID`'s session: the
// root thread's, and those of each agent thread that runs a live command.
// Each Codex thread has its own processes, so a cleanup of the root thread
// alone leaves the agents' running. Thread delete uses it, and a revert
// starts with it (stopCodexCommandsBeforeRevert).
//
// Codex answers before the processes are gone, so the answer ends no
// command: each leaves the tray by its own item/completed, by its thread's
// end, or when a list no longer holds it (triage codexExecTrackers). An
// agent thread that is already gone has no processes left; its error is
// read, and the commands it ran end with it.
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
//
//ao:scope threads:operate
func (a *App) CleanCodexBackgroundTerminals(threadID string) error {
	if a.shuttingDown.Load() {
		return ErrShuttingDown
	}
	ctx, cancel := context.WithTimeout(context.Background(), codexapp.BackgroundTerminalStopTimeout)
	defer cancel()
	_, err := a.cleanCodexScopes(ctx, threadID, nil)
	return err
}

// cleanCodexScopes cleans the root thread's processes, then those of each
// Codex thread that runs a live command started after cleaned last cleaned
// it, and returns what it cleaned: per scope, the latest command start the
// clean covers. A nil cleaned cleans every scope with a live command.
func (a *App) cleanCodexScopes(ctx context.Context, threadID string, cleaned map[string]uint64) (map[string]uint64, error) {
	codexApp := a.codexAppService()
	if cleaned == nil {
		root, err := codexApp.RootProcessScope(threadID)
		if err != nil {
			return nil, err
		}
		var pin triage.CodexScopeCheck
		if a.triage != nil {
			pin = a.triage.PinCodexScopeCheck(threadID, root)
		}
		if err := codexApp.CleanBackgroundTerminals(ctx, threadID, root); err != nil {
			return nil, err
		}
		cleaned = map[string]uint64{root: pin.Through}
	}
	if a.triage == nil {
		return cleaned, nil
	}
	newest := map[string]triage.CodexCommand{}
	for _, command := range a.triage.LiveCodexCommands(threadID) {
		if !command.Addressable || command.Seq <= cleaned[command.ScopeID] {
			continue
		}
		if prior, ok := newest[command.ScopeID]; !ok || command.Seq > prior.Seq {
			newest[command.ScopeID] = command
		}
	}
	var errs []error
	for scopeID, command := range newest {
		err := codexApp.CleanBackgroundTerminals(ctx, threadID, scopeID)
		if errors.Is(err, codex.ErrBackgroundTerminalThreadNotLoaded) {
			a.triage.ConfirmCodexScope(threadID, command.Check, nil, true)
			err = nil
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		cleaned[scopeID] = command.Check.Through
	}
	return cleaned, errors.Join(errs...)
}

// StopCodexBackgroundCommand stops one running Codex command, named by its
// item id (the tray row's id), in the Codex thread whose processes include
// it: the root thread's, or the agent's that ran it. It is the Codex
// counterpart of StopClaudeTask.
//
// true: Codex terminated the process, and its item/completed takes the row
// out of the tray. false, nil: the command had already ended; it has left
// the tray. An error says why it could not be stopped: a command inside a
// code review ends with the review, and one Codex still runs after the stop
// is named so the person can try again.
//
//ao:scope threads:operate
func (a *App) StopCodexBackgroundCommand(threadID, launchID string) (bool, error) {
	if a.shuttingDown.Load() {
		return false, ErrShuttingDown
	}
	if a.triage == nil {
		return false, fmt.Errorf("app: stop codex command: thread %s has no event router", threadID)
	}
	command, ok := a.triage.LiveCodexCommand(threadID, launchID)
	if !ok {
		return false, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), codexapp.BackgroundTerminalStopTimeout)
	defer cancel()
	return a.stopCodexCommand(ctx, threadID, command)
}

// TerminateCodexBackgroundTerminal stops ONE running command of the root
// Codex thread, identified by the app-server process id its tray row
// carries as `meta.process_id`. It is the Stop of a window older than
// StopCodexBackgroundCommand; a row carries a process id only for a root
// command, the only one this id can name. Each Codex thread numbers its own
// processes, so an agent's command is stopped by item id. Within the root
// thread, a process id Codex reused after its command ended names the
// newer command.
//
// The bool is the stop's answer, as StopCodexBackgroundCommand's: false,
// nil also means no live root command holds the id, and nothing was sent.
//
//ao:scope threads:operate
func (a *App) TerminateCodexBackgroundTerminal(threadID, processID string) (bool, error) {
	if a.shuttingDown.Load() {
		return false, ErrShuttingDown
	}
	if a.triage == nil {
		return false, fmt.Errorf("app: terminate codex background terminal: thread %s has no event router", threadID)
	}
	command, ok := a.triage.LiveRootCodexCommandForProcess(threadID, processID)
	if !ok {
		return false, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), codexapp.BackgroundTerminalStopTimeout)
	defer cancel()
	return a.stopCodexCommand(ctx, threadID, command)
}

// stopCodexCommand terminates command's process in the Codex thread that
// runs it. Codex's false is no proof the process is gone (an exited
// process the model polled has left the table before its end is written),
// so false is checked against that thread's list: absent, the command has
// ended (false, nil); still listed, the stop failed and says so.
func (a *App) stopCodexCommand(ctx context.Context, threadID string, command triage.CodexCommand) (bool, error) {
	if command.ScopeID == "" {
		return false, fmt.Errorf("app: stop codex command %q: Codex did not say which thread runs it", command.Summary)
	}
	if !command.Addressable {
		return false, fmt.Errorf("app: stop codex command %q: it runs inside the code review and ends with the review", command.Summary)
	}
	if command.ProcessID == "" {
		return false, fmt.Errorf("app: stop codex command %q: Codex has not named its process yet", command.Summary)
	}
	terminated, err := a.codexAppService().TerminateBackgroundTerminal(ctx, threadID, command.ScopeID, command.ProcessID)
	notLoaded := errors.Is(err, codex.ErrBackgroundTerminalThreadNotLoaded)
	if err != nil && !notLoaded {
		return false, fmt.Errorf("app: stop codex command %q: %w", command.Summary, err)
	}
	if terminated {
		return true, nil
	}
	if err := a.confirmCodexScope(ctx, threadID, command.Check, notLoaded); err != nil {
		return false, fmt.Errorf("app: stop codex command %q: Codex found no process to stop, and listing its processes failed: %w", command.Summary, err)
	}
	if _, live := a.triage.LiveCodexCommand(threadID, command.LaunchID); live {
		return false, fmt.Errorf("app: stop codex command %q: Codex did not stop it, and it is still running", command.Summary)
	}
	log.Printf("codex commands: thread %s: command %s had already ended when its stop was sent", threadID, command.LaunchID)
	return false, nil
}

// confirmCodexScope asks Codex which of check's scope's processes run, and
// hands the answer to triage, which ends the commands it no longer holds
// (ConfirmCodexScope). notLoaded skips the list: Codex already answered
// that the thread is gone.
func (a *App) confirmCodexScope(ctx context.Context, threadID string, check triage.CodexScopeCheck, notLoaded bool) error {
	var listed []string
	if !notLoaded {
		terminals, err := a.codexAppService().ListBackgroundTerminals(ctx, threadID, check.ScopeID)
		switch {
		case errors.Is(err, codex.ErrBackgroundTerminalThreadNotLoaded):
			notLoaded = true
		case err != nil:
			return err
		}
		for _, terminal := range terminals {
			listed = append(listed, terminal.ItemID)
		}
	}
	if ended := a.triage.ConfirmCodexScope(threadID, check, listed, notLoaded); len(ended) > 0 {
		log.Printf("codex commands: thread %s: Codex no longer runs %v in %s; they left the live work before their item/completed", threadID, ended, check.ScopeID)
	}
	return nil
}

// runCodexScopeCheck answers the scope check triage asks for when a close
// item names an agent that runs commands (triage.SetCodexScopeCheckHook).
// It runs off the provider event worker, on joined work.
func (a *App) runCodexScopeCheck(threadID string, check triage.CodexScopeCheck) {
	started := a.codexScopeChecks.Go(func() {
		ctx, cancel := context.WithTimeout(context.Background(), codexapp.BackgroundTerminalStopTimeout)
		defer cancel()
		if err := a.confirmCodexScope(ctx, threadID, check, false); err != nil {
			log.Printf("codex commands: thread %s: check of agent thread %s after its close failed; its commands stay live: %v", threadID, check.ScopeID, err)
		}
	})
	if !started {
		log.Printf("codex commands: thread %s: shutting down, so agent thread %s is not checked after its close", threadID, check.ScopeID)
	}
}

// StopCodexSubagent interrupts the live child turn owned by launchID. The
// provider resolves the launch through its typed child-ownership map.
//
//ao:scope threads:operate
func (a *App) StopCodexSubagent(threadID, launchID string) (bool, error) {
	if a.shuttingDown.Load() {
		return false, ErrShuttingDown
	}
	ctx, cancel := context.WithTimeout(context.Background(), codexapp.BackgroundTerminalStopTimeout)
	defer cancel()
	return a.codexAppService().StopSubagent(ctx, threadID, launchID)
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
//
//ao:scope threads:operate
//ao:route selected
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
//
//ao:scope access:admin
//ao:route home
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
		a.codexApp = codexapp.New(a.codexAppDeps())
	})
	return a.codexApp
}

func (a *App) codexAppDeps() codexapp.Deps {
	return codexapp.Deps{
		Reaper: a.groupReaper("codex read"),
		Session: func(threadID string) (*codex.Session, bool) {
			session, active := a.sessionManager().get(threadID)
			return session.Codex, active
		},
		ConnForBinary:    a.borrowCodexConnForBinary,
		AnyConn:          a.borrowCodexConnForAccount,
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
	}
}

func (a *App) handleCodexSkillsChanged() {
	a.codexAppService().ResetSkills()
}

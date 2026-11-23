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
// command: each leaves the tray by its own item/completed or by its
// thread's unload (triage codexExecTrackers). An agent thread that is
// already gone answers "thread not found", which is that unload.
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
	session, err := a.codexCommandSession("clean codex background terminals", threadID, "")
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), codexapp.BackgroundTerminalStopTimeout)
	defer cancel()
	_, err = a.cleanCodexScopes(ctx, threadID, session, nil)
	return err
}

// codexCommandSession returns the Codex session that runs threadID's
// thread, and refuses when token is set and names another session: a
// command's stop goes to the session triage read the command in and to no
// other, since a replacement's process table is another table.
func (a *App) codexCommandSession(action, threadID, token string) (*codex.Session, error) {
	entry, active := a.sessionManager().get(threadID)
	switch {
	case !active:
		return nil, fmt.Errorf("app: %s: no active session for thread %s", action, threadID)
	case entry.Codex == nil:
		return nil, fmt.Errorf("app: %s: thread %s is not a Codex thread", action, threadID)
	case token != "" && entry.Token != token:
		return nil, fmt.Errorf("app: %s: the session that ran it has ended", action)
	}
	return entry.Codex, nil
}

// cleanCodexScopes cleans, on session, the processes of the root thread when
// cleaned is nil, and of each Codex thread that runs a live command cleaned
// does not hold. It returns cleaned with the commands its cleans covered: a
// clean covers the commands that had started when it was sent.
func (a *App) cleanCodexScopes(ctx context.Context, threadID string, session *codex.Session, cleaned map[string]bool) (map[string]bool, error) {
	scopes := map[string][]string{}
	if cleaned == nil {
		root := session.RootProcessScope()
		if root.ID == "" {
			return nil, fmt.Errorf("app: clean codex background terminals: thread %s has no Codex thread yet", threadID)
		}
		cleaned = map[string]bool{}
		scopes[root.ID] = nil
	}
	if a.triage != nil {
		for _, command := range a.triage.LiveCodexCommands(threadID) {
			if command.Addressable && !cleaned[command.LaunchID] {
				scopes[command.ScopeID] = append(scopes[command.ScopeID], command.LaunchID)
			}
		}
	}
	var errs []error
	for scopeID, launches := range scopes {
		err := session.CleanBackgroundTerminals(ctx, scopeID)
		if errors.Is(err, codex.ErrBackgroundTerminalThreadNotLoaded) {
			// The thread's unload, which ends its commands.
			err = nil
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, launchID := range launches {
			cleaned[launchID] = true
		}
	}
	return cleaned, errors.Join(errs...)
}

// StopCodexBackgroundCommand stops one running Codex command, named by its
// item id (the tray row's id), in the Codex thread whose processes include
// it: the root thread's, or the agent's that ran it. It is the Codex
// counterpart of StopClaudeTask.
//
// true: Codex terminated the process, and its item/completed takes the row
// out of the tray. false, nil: the command had already ended. An error says
// why it was not stopped: Codex did not stop it, the command runs inside a
// code review and ends with the review, or its process is no longer the one
// the row names. The row stays until the command ends.
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
// runs it, on the session triage read command in. Just before it sends, it
// checks that this session still runs the thread and that command still
// holds its process id in its table (triage CodexCommandHoldsProcess):
// Codex gives an exited process's id to the next process it starts, which
// can be before the exited command's completion arrives. A process that
// exits after that check, and whose id Codex gives to a new process before
// it reads the request, is the window no check on this side closes; it is
// as long as one request's trip.
//
// Codex's false means it did not stop the process: it had none by that id,
// or the terminate failed (core unified_exec process_manager.rs
// terminate_process). The command stays live until its own completion or
// its thread's unload says it ended, and the error says Codex did not stop
// it. A thread Codex no longer has loaded answers "thread not found", which
// is its unload: the command has ended.
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
	session, err := a.codexCommandSession(fmt.Sprintf("stop codex command %q", command.Summary), threadID, command.Token)
	if err != nil {
		return false, err
	}
	if !a.triage.CodexCommandHoldsProcess(threadID, command) {
		if _, live := a.triage.LiveCodexCommand(threadID, command.LaunchID); !live {
			return false, nil
		}
		return false, fmt.Errorf("app: stop codex command %q: its process has exited and Codex gave process %s to another command; the row goes when Codex reports the end", command.Summary, command.ProcessID)
	}
	terminated, err := session.TerminateBackgroundTerminal(ctx, command.ScopeID, command.ProcessID)
	switch {
	case errors.Is(err, codex.ErrBackgroundTerminalThreadNotLoaded):
		log.Printf("codex commands: thread %s: command %s's agent thread is gone, and the command with it", threadID, command.LaunchID)
		return false, nil
	case err != nil:
		return false, fmt.Errorf("app: stop codex command %q: %w", command.Summary, err)
	case terminated:
		return true, nil
	default:
		return false, fmt.Errorf("app: stop codex command %q: Codex did not stop it; the row goes when Codex reports the command ended", command.Summary)
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

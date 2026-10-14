package app

import (
	"fmt"
	"log"
	"os"
	"runtime"
	"strings"
	"sync"

	"agent-overflow/internal/gitroot"
	"agent-overflow/internal/promptoverride"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/settings"
	"agent-overflow/internal/store"

	"github.com/shirou/gopsutil/v4/host"
)

// The settings-level system-prompt override and tool toggles
// (docs/specs/prompt-tool-overrides.md).
//
// Both axes are owned by Settings rather than by the thread row, which is
// what makes the two entry points below a PAIR. applySettingsOwnedAxes
// stamps today's values on the spawn path; reconcileSettingsOwnedAxes
// carries the live session's forward on the reconcile path. Adding a
// settings-owned axis means touching BOTH: an axis stamped only on the
// spawn side would queue a deferred restart on every live session the next
// time anything reconciled.
//
// The tool lists and the system prompt are pinned outright on the reconcile
// path: no control_request can add or drop a tool mid-session, and a prompt
// change applies only at the thread's next session start (docs/decisions.md
// 2026-10-14). The THINKING axis is the opposite: it is re-read on both
// paths and costs nothing to resolve (a struct copy), because
// `set_max_thinking_tokens` lands a budget or an off switch on a running
// session. Only its return to "Claude Code decides" has no wire form, and
// that one direction falls through to the deferred restart.
//
// Whether a live session's prompt still matches what its next start would
// use is answered separately, by reconcileStalePromptSessions, on the
// stored, unrendered entry text (SessionOptions.SystemPromptOverrideSource).
// Comparing rendered prompts would report a change every time the
// workspace's git state moved under a {{GIT_BLOCK}}.

// claudeThinkingOption crosses the settings → provider boundary for the
// thinking axis. Two structurally identical types exist because
// internal/settings may not import internal/provider (see that package's
// AGENTS.md §Anti-patterns), and this is the single conversion site, so a
// field added on one side that is not carried here is a compile error at
// neither — which is why the round-trip is pinned by a test.
func claudeThinkingOption(thinking settings.ClaudeThinking) provider.ClaudeThinking {
	return provider.ClaudeThinking{
		Mode:         thinking.Mode,
		BudgetTokens: thinking.BudgetTokens,
		Display:      thinking.Display,
	}
}

// promptOverrideResolution is the single decision a spawn makes about the
// settings-level override, so every downstream consumer reads the same
// answer from the same settings snapshot. Applied is false when a feature
// owns the prompt or no entry matched — in which case Entry is the zero
// value and nothing about the override is in play for this session.
type promptOverrideResolution struct {
	Entry   settings.PromptOverride
	Applied bool
}

// applySettingsOwnedAxes stamps the axes Settings owns onto a freshly
// built options bundle: the system-prompt override (only when no feature
// already claimed the prompt), the provider's disabled-tool list, the
// todo-reminder and thinking axes, and the Claude cross-session peer
// inbox plus the name this thread advertises to peers.
//
// SPAWN PATH ONLY — it renders placeholders, which costs a git probe, and
// its result is what the live session is then pinned to. The reconcile path
// calls reconcileSettingsOwnedAxes instead.
//
// One settings snapshot serves both axes and the returned resolution, so a
// save landing mid-spawn cannot produce a session whose prompt, tool list,
// and memory directory came from three different versions of the file.
//
// opts.WorkDir must already be final: a prompt
// that describes a directory the session is not in is worse than one that
// describes none.
func (a *App) applySettingsOwnedAxes(t store.Thread, opts *provider.SessionOptions) (promptOverrideResolution, error) {
	snapshot := a.settings.Get()

	// Provider-wide and settings-owned, so it is stamped here rather than
	// projected from the thread row — same reasoning as FastModeTierID.
	opts.DisabledTools = snapshot.DisabledToolsForProvider(t.Provider)
	opts.DisableTodoReminders = snapshot.TodoRemindersDisabledForProvider(t.Provider)
	opts.ClaudeThinking = claudeThinkingOption(snapshot.ClaudeThinkingForProvider(t.Provider))
	opts.ClaudeCrossSession = claudeCrossSessionOption(snapshot.ClaudeCrossSessionForProvider(t.Provider))
	// Derived even when the inbox is off, and harmlessly so: the name only
	// reaches argv when cross-session messaging is enabled
	// (claude.peerSessionNameArgs), and deriving it unconditionally keeps
	// one code path rather than two that can disagree.
	opts.ClaudePeerSessionName = a.peerSessionNameForThread(t)

	// A non-empty prompt at this point means a FEATURE owns it (the
	// discussion deliberation prompt — see
	// featureOwnedSystemPrompt, the one definition of that precedence).
	// Those features already run fully custom prompts and replacing one
	// would break them.
	if opts.SystemPrompt != "" {
		return promptOverrideResolution{}, nil
	}
	entry, ok := promptoverride.Match(
		snapshot.PromptOverridesForProvider(t.Provider),
		t.Provider,
		t.Model,
	)
	if !ok {
		return promptOverrideResolution{}, nil
	}

	rendered := promptoverride.Render(entry.Prompt, a.promptOverrideFacts(t, entry.Prompt, opts.WorkDir))
	if len(rendered) > promptoverride.MaxRenderedPromptBytes {
		return promptOverrideResolution{}, fmt.Errorf(
			"system prompt override renders to %d bytes from a %d-byte prompt, over the %d-byte limit: "+
				"check for a repeated {{GIT_BLOCK}} placeholder",
			len(rendered), len(entry.Prompt), promptoverride.MaxRenderedPromptBytes,
		)
	}
	opts.SystemPrompt = rendered
	opts.SystemPromptOverrideSource = entry.Prompt
	return promptOverrideResolution{Entry: entry, Applied: true}, nil
}

// reconcileSettingsOwnedAxes resolves the settings-owned axes of a LIVE
// session onto a freshly built options bundle, so the config reconciler
// diffs only what may legitimately have changed.
//
// RECONCILE PATH ONLY. The tool lists, the todo-reminder switch and the
// settings-level system prompt are pinned to what the session launched
// with on every provider. A prompt change never applies to a running
// session (docs/decisions.md 2026-10-14): it applies at the thread's next
// session start, and reconcileStalePromptSessions tells the user which
// live threads differ. A non-empty opts.SystemPrompt after
// buildSessionOptions means a feature owns it (currently discussions);
// that one converges by restart exactly as it always has, so it is left
// alone. Nothing here matches or renders an override.
func (a *App) reconcileSettingsOwnedAxes(t store.Thread, opts *provider.SessionOptions, launch provider.SessionOptions) {
	opts.DisabledTools = launch.DisabledTools
	opts.DisableTodoReminders = launch.DisableTodoReminders
	// PINNED, and not for the usual reason: ConfigFromOptions never reads
	// this field, so a fresh value could not queue a restart even if it
	// wanted to. It is carried forward so launchOpts keeps describing the
	// process that is actually running; the name converges live through
	// syncPeerSessionName instead.
	opts.ClaudePeerSessionName = launch.ClaudePeerSessionName
	if opts.SystemPrompt == "" {
		opts.SystemPrompt = launch.SystemPrompt
		opts.SystemPromptOverrideSource = launch.SystemPromptOverrideSource
	}

	// Only headless Claude reacts to the remaining axes; Codex and
	// claude-tui keep their zero values on both paths.
	if t.Provider != string(provider.Claude) {
		return
	}
	// One snapshot for both axes, so a save landing between two reads
	// cannot hand the session thinking and inbox values from different
	// versions of the settings.
	snapshot := a.settings.Get()
	// RESOLVED, not pinned: a Settings save converges the running session
	// through claude.PlanLiveUpdate's set_max_thinking_tokens. The return
	// to "Claude Code decides" has no wire form and falls through to the
	// deferred restart.
	opts.ClaudeThinking = claudeThinkingOption(snapshot.ClaudeThinkingForProvider(t.Provider))
	// Also RESOLVED rather than pinned, but converging the opposite way:
	// nothing rebinds the peer inbox on a running process, so a change
	// here falls through PlanLiveUpdate's trailing DeepEqual and becomes a
	// DEFERRED restart. Pinning it would make the setting silently never
	// converge.
	opts.ClaudeCrossSession = claudeCrossSessionOption(snapshot.ClaudeCrossSessionForProvider(t.Provider))
}

// featureOwnedSystemPrompt returns the prompt a feature owns for this thread:
// currently the discussion deliberation prompt. Non-empty means the
// settings-level override is skipped — those features already run a fully
// custom prompt and replacing it would break them.
//
// The one definition of that precedence rule, and it is evaluated exactly
// once per build (in buildSessionOptions). Everything downstream reads the
// answer off opts.SystemPrompt instead: after the build, non-empty means
// "a feature owns this" and empty means "the settings override may claim
// it". Re-deriving it later is how the two sides drift.
func (a *App) featureOwnedSystemPrompt(t store.Thread) string {
	return a.threadSystemPrompt(t.ID)
}

// promptOverrideFacts gathers the spawn-time values the placeholders
// render to. Every fact is computed only when the prompt actually asks for
// it — the git facts cost a subprocess pair and the host version reads
// several files, and a prompt that never mentions them must not pay.
func (a *App) promptOverrideFacts(t store.Thread, prompt, workDir string) promptoverride.Facts {
	facts := promptoverride.Facts{WorkDir: workDir}

	if promptoverride.Uses(prompt, promptoverride.TokenPlatform) {
		facts.Platform = runtime.GOOS
	}
	if promptoverride.Uses(prompt, promptoverride.TokenOSVersion) {
		facts.OSVersion = hostOSVersion()
	}
	if promptoverride.Uses(prompt, promptoverride.TokenModelID) {
		facts.ModelID = t.Model
	}
	if promptoverride.Uses(prompt, promptoverride.TokenModelName) {
		facts.ModelName = modelDisplayName(t.Provider, t.Model)
	}
	usesBlock := promptoverride.Uses(prompt, promptoverride.TokenGitBlock)
	if usesBlock || promptoverride.Uses(prompt, promptoverride.TokenIsGitRepo) {
		facts.IsGitRepo, facts.GitBlock = a.promptOverrideGitFacts(workDir, usesBlock)
	}
	if promptoverride.Uses(prompt, promptoverride.TokenMemoryDir) {
		if dir, _, err := a.claudeMemoryDirForThread(t, workDir); err == nil {
			facts.MemoryDir = dir
		}
	}
	return facts
}

// promptOverrideGitFacts answers the two git placeholders for a workspace.
// A probe failure degrades to "not a repo" with a log line rather than
// failing the spawn: an unreadable git directory is a reason for a thinner
// prompt, never a reason the session cannot start.
//
// needBlock is what the prompt asked for, and it decides which probe runs.
// {{IS_GIT_REPO}} alone is answerable from git's own on-disk layout
// (gitroot.MainRoot walks up for a .git directory or worktree pointer,
// resolving a linked worktree to the repository it was cut from), so the
// common "is this a repo" prompt costs zero subprocesses. Only
// {{GIT_BLOCK}} needs the two-subprocess PromptSnapshot.
func (a *App) promptOverrideGitFacts(workDir string, needBlock bool) (isRepo, block string) {
	if strings.TrimSpace(workDir) == "" {
		return "No", ""
	}
	if !needBlock {
		if _, ok := gitroot.MainRoot(workDir); !ok {
			return "No", ""
		}
		return "Yes", ""
	}
	snapshot, err := a.gitCore().PromptSnapshot(workDir)
	if err != nil {
		log.Printf("prompt override: git snapshot for %s: %v — rendering as non-repo", workDir, err)
		return "No", ""
	}
	if !snapshot.IsRepo {
		return "No", ""
	}
	return "Yes", snapshot.PromptBlock()
}

// claudeMemoryDirForThread resolves the Claude memory directory for a
// thread's workspace. Both Claude transports have one — headless and the
// interactive TUI are the same binary reading the same
// `<claudeHome>/projects/<slug>/memory`, and the CLI stops creating it
// under a replaced system prompt on either. Codex threads have none — the
// placeholder renders empty there rather than pointing a Codex session at
// a Claude path.
//
// The home is App.providerHome() — ordinarily the OS home, because that
// is what the spawned CLI itself resolves: the headless spawn clears
// CLAUDE_CONFIG_DIR outright and both Claude providers refuse it from a
// user's custom environment (provider.ReservedEnvNames), so `~/.claude`
// under $HOME is the home the session will read its memory from. Under an
// isolated boot it is the pinned harness home, so the mkdir below can
// never reach the developer's real `~/.claude/projects`.
//
// The bool means "this thread HAS a memory directory" — false for a Codex
// thread or a workspace-less one, both of which are ordinary states rather
// than failures. Every workspace that reaches promptoverride.ClaudeMemoryDir
// resolves, so beyond those two guards only the error answers false.
func (a *App) claudeMemoryDirForThread(t store.Thread, workDir string) (string, bool, error) {
	if t.Provider != string(provider.Claude) && t.Provider != string(provider.ClaudeTUI) {
		return "", false, nil
	}
	if strings.TrimSpace(workDir) == "" {
		return "", false, nil
	}
	home, err := a.providerHome()
	if err != nil {
		return "", false, err
	}
	dir, err := promptoverride.ClaudeMemoryDir(home, workDir)
	if err != nil {
		return "", false, err
	}
	return dir, true, nil
}

// ensureClaudeMemoryDir creates the memory directory a rendered override
// promised the model already exists. Under a replaced system prompt the CLI
// stops creating it (verified 2.1.234) while memory RECALL keeps working,
// so this is what makes the write side of the feature real.
//
// Runs on the spawn path, the one place an override is rendered, and takes
// the resolution its renderer already made rather than re-matching: a
// settings save landing between the two would otherwise let the mkdir act
// on a different entry than the one whose {{MEMORY_DIR}} was rendered into
// the prompt.
//
// Never fails the spawn: a session without a memory directory is a degraded
// session, not a broken one, so the failure lands as thread error state
// (which the user can see and act on) and startup continues.
func (a *App) ensureClaudeMemoryDir(t store.Thread, workDir string, res promptOverrideResolution) {
	if !res.Applied || !promptoverride.Uses(res.Entry.Prompt, promptoverride.TokenMemoryDir) {
		return
	}
	dir, resolved, err := a.claudeMemoryDirForThread(t, workDir)
	if err != nil {
		log.Printf("thread %s: resolve Claude memory dir: %v", t.ID, err)
		a.emitErrorToThread(t.ID, "system prompt override: could not resolve the memory directory: "+err.Error())
		return
	}
	if !resolved {
		return
	}
	// 0700 matches what the CLI creates the directory tree with, and the
	// transcripts already living beside it are private by the same rule.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		log.Printf("thread %s: create Claude memory dir %s: %v", t.ID, dir, err)
		a.emitErrorToThread(t.ID, "system prompt override: could not create the memory directory "+dir+": "+err.Error())
	}
}

func modelDisplayName(providerName, model string) string {
	if info, ok := provider.FindModel(providerName, model); ok && info.Name != "" {
		return info.Name
	}
	return provider.NormalizeModelSlug(providerName, model)
}

var (
	osVersionOnce  sync.Once
	osVersionValue string
)

// hostOSVersion returns a human-readable OS version ("ubuntu 24.04",
// "darwin 15.3"). Cached: it cannot change while the process runs, and
// the probe reads several files.
func hostOSVersion() string {
	osVersionOnce.Do(func() {
		platform, _, version, err := host.PlatformInformation()
		if err != nil {
			log.Printf("prompt override: read host platform information: %v", err)
			osVersionValue = runtime.GOOS
			return
		}
		osVersionValue = strings.TrimSpace(platform + " " + version)
		if osVersionValue == "" {
			osVersionValue = runtime.GOOS
		}
	})
	return osVersionValue
}

func (a *App) threadSystemPrompt(threadID string) string {
	return a.sessionManager().runtime.ThreadSystemPrompt(threadID)
}

func (a *App) setThreadSystemPrompt(threadID, prompt string) {
	a.sessionManager().runtime.SetThreadSystemPrompt(threadID, prompt)
}

func (a *App) clearThreadSystemPrompt(threadID string) {
	a.sessionManager().runtime.ClearThreadSystemPrompt(threadID)
}

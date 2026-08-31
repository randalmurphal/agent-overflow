package main

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
// The tool lists are pinned outright — no control_request can add or drop a
// tool mid-session, so re-reading them on the reconcile path could only
// produce restarts nobody asked for. The THINKING axis is the opposite: it
// is re-read on both paths and costs nothing to resolve (a struct copy),
// because `set_max_thinking_tokens` lands a budget or an off switch on a
// running session. Only its return to "Claude Code decides" has no wire
// form, and that one direction falls through to the deferred restart — the
// same asymmetry the prompt override has. The system prompt is NOT pinned
// either, since
// `set_model.system_prompt` can swap one live (internal/provider/claude
// live_update.go). It converges on an EDIT to the stored override and on
// nothing else: reconcileSettingsOwnedAxes compares the stored, unrendered
// entry text against what this session launched with
// (SessionOptions.SystemPromptOverrideSource) and only re-renders when that
// changed. Comparing rendered prompts instead would re-render — two git
// subprocesses, under the per-thread config lock — on every model, effort,
// and runtime-mode change, and a prompt using {{GIT_BLOCK}} would report a
// diff every time the workspace's git state moved.

// maxRenderedSystemPromptBytes bounds what a rendered override may grow to.
// The stored prompt is already length-validated in Settings, but rendering
// is multiplicative: {{GIT_BLOCK}} expands to a repository snapshot, so a
// stored prompt repeating the token can render to tens of megabytes — every
// byte of which is context the model pays for on every turn, and which the
// user never sees before the session starts. 256KB is far above any real
// prompt (Claude's own default body is ~12KB) and far below the point where
// a session becomes unusable. Exceeding it fails the spawn with both sizes
// named; truncating instead would hand the model a prompt cut mid-sentence.
const maxRenderedSystemPromptBytes = 256_000

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
	if len(rendered) > maxRenderedSystemPromptBytes {
		return promptOverrideResolution{}, fmt.Errorf(
			"system prompt override renders to %d bytes from a %d-byte prompt, over the %d-byte limit: "+
				"check for a repeated {{GIT_BLOCK}} placeholder",
			len(rendered), len(entry.Prompt), maxRenderedSystemPromptBytes,
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
// RECONCILE PATH ONLY. The tool lists are pinned to what the session
// launched with (spawn-only on every provider — a diff there could only
// queue a restart the user never asked for). The prompt is resolved:
//
//  1. A non-empty opts.SystemPrompt after buildSessionOptions means a
//     feature owns it (currently discussions). Those converge exactly as
//     they always have — nothing to do.
//  2. Otherwise the settings-level override decides. Today's entry is
//     MATCHED (cheap — a slug comparison, no git, no render) and its STORED
//     text compared against the one this session launched with. Unchanged
//     is the overwhelmingly common case and pins, so the reconciler stays
//     free of Render and the git probe on every model / effort /
//     runtime-mode change.
//  3. Only when the stored text actually changed — a Settings edit, an
//     entry enabled or disabled, or the thread's model moving to a
//     different entry's scope — is the override rendered, which is where
//     the git subprocesses live. Any change that LANDS on a non-empty
//     prompt — an edit, or an override newly turned on — is then a live
//     `set_model.system_prompt`; only turning one OFF is a deferred
//     restart, because the CLI has no revert-to-built-in form for the
//     field. All three verdicts belong to claude.PlanLiveUpdate.
//
// Returns the resolution behind a NEWLY RENDERED prompt (Applied false in
// every other case, including the pinned one) so the caller can create the
// memory directory the new prompt may claim exists.
func (a *App) reconcileSettingsOwnedAxes(
	t store.Thread,
	sessionToken string,
	opts *provider.SessionOptions,
	launch provider.SessionOptions,
) promptOverrideResolution {
	// ONE snapshot for every axis below, exactly as the spawn path takes
	// one: three separate Get() calls could straddle a save and produce a
	// session whose thinking, inbox and prompt came from different
	// versions of the file.
	snapshot := a.settings.Get()

	opts.DisabledTools = launch.DisabledTools
	opts.DisableTodoReminders = launch.DisableTodoReminders
	// PINNED, and not for the usual reason: ConfigFromOptions never reads
	// this field, so a fresh value could not queue a restart even if it
	// wanted to. It is carried forward so launchOpts keeps describing the
	// process that is actually running; the name converges live through
	// syncPeerSessionName instead.
	opts.ClaudePeerSessionName = launch.ClaudePeerSessionName

	// Only headless Claude reacts to any of the remaining axes, so the
	// provider gate comes BEFORE them: Codex and claude-tui would
	// otherwise pay for Claude-only lookups whose result they pin anyway.
	// A prompt swap without a restart is `set_model.system_prompt`, which
	// neither of them has — re-resolving there could only ever queue a
	// deferred restart for a Settings edit the user expected to affect the
	// NEXT session, the contract those two keep.
	//
	// The pin is CONDITIONAL for the same reason it is on the Claude side
	// below: SystemPrompt is a shared axis, and a non-empty value after
	// buildSessionOptions means a feature owns it (currently the discussion
	// deliberation prompt). Those converge by
	// restart exactly as they always have, and pinning over one would
	// freeze a feature-owned prompt at its first value forever.
	if t.Provider != string(provider.Claude) {
		if opts.SystemPrompt == "" {
			opts.SystemPrompt = launch.SystemPrompt
			opts.SystemPromptOverrideSource = launch.SystemPromptOverrideSource
		}
		return promptOverrideResolution{}
	}

	// RESOLVED, not pinned — the one axis here that a live session can
	// adopt outright. It is read fresh (a struct copy: no match, no
	// render, no subprocess) so a Settings save converges the running
	// session through claude.PlanLiveUpdate's set_max_thinking_tokens.
	// The return to "Claude Code decides" is the one direction that
	// cannot, and it falls through to the deferred restart exactly like
	// turning a prompt override off.
	opts.ClaudeThinking = claudeThinkingOption(snapshot.ClaudeThinkingForProvider(t.Provider))
	// Also RESOLVED rather than pinned, but converging the opposite way:
	// nothing rebinds the peer inbox on a running process, so a change
	// here falls through PlanLiveUpdate's trailing DeepEqual and becomes a
	// DEFERRED restart — which is the whole reason this axis rides
	// SessionOptions instead of being stamped onto claude.Config beside
	// the other `--settings` axes. Pinning it would make the setting
	// silently never converge.
	opts.ClaudeCrossSession = claudeCrossSessionOption(snapshot.ClaudeCrossSessionForProvider(t.Provider))

	if opts.SystemPrompt != "" {
		return promptOverrideResolution{}
	}

	entry, matched := promptoverride.Match(
		snapshot.PromptOverridesForProvider(t.Provider),
		t.Provider,
		t.Model,
	)
	source := ""
	if matched {
		source = entry.Prompt
	}
	if source == launch.SystemPromptOverrideSource {
		// Same stored override (or the same absence of one). Pin, and pay
		// for nothing.
		opts.SystemPrompt = launch.SystemPrompt
		opts.SystemPromptOverrideSource = launch.SystemPromptOverrideSource
		return promptOverrideResolution{}
	}
	if !matched {
		// The override was turned off or stopped matching this model. Leave
		// the prompt empty: that is a non-empty → empty transition, the one
		// direction PlanLiveUpdate refuses (there is no revert-to-built-in
		// wire form), and the restart converges honestly.
		opts.SystemPromptOverrideSource = ""
		return promptOverrideResolution{}
	}

	rendered, ok := a.reconcilePromptOverrideRender(t, sessionToken, entry, opts.WorkDir)
	if !ok {
		// Over the size limit. A reconcile must not be fatal the way a
		// spawn is — killing a live session over a Settings edit is the
		// worse outcome, and the restart path could only respawn into the
		// same refusal — so the live session's prompt is pinned. The
		// refusal is NOT silent, though: pinning means the session keeps
		// running the OLD prompt while Settings shows the new one, and a
		// user who is not told believes the prompt they just saved is
		// active. reconcilePromptOverrideRender surfaces it as thread error
		// state (once per verdict, not once per reconcile poll).
		opts.SystemPrompt = launch.SystemPrompt
		opts.SystemPromptOverrideSource = launch.SystemPromptOverrideSource
		return promptOverrideResolution{}
	}
	opts.SystemPrompt = rendered
	opts.SystemPromptOverrideSource = entry.Prompt
	return promptOverrideResolution{Entry: entry, Applied: true}
}

// promptOverrideRender memoizes one reconcile-path render for one live
// session. Keyed by everything the render depends on that can move under a
// live session: the stored override text, the workspace, and the model
// (whose id and display name are placeholders). The remaining facts —
// platform, OS version, memory dir — are functions of those or of the
// process itself.
//
// Deliberately NOT keyed on git state, which is the whole point: a
// {{GIT_BLOCK}} prompt must not re-render because someone committed. The
// spawn path is where a session picks up a fresh repository snapshot.
type promptOverrideRender struct {
	source   string
	workDir  string
	model    string
	rendered string
	// oversize records that this key rendered OVER the size limit. The
	// verdict is memoized exactly like a successful render, for both of
	// the memo's reasons: the git subprocesses must not be re-paid on
	// every reconcile poll, and the user must not be told the same thing
	// once a second. rendered is empty when it is set.
	oversize bool
}

// reconcilePromptOverrideRender renders a matched override for the reconcile
// path, reusing the previous render when nothing it depends on moved.
//
// The memo is what keeps the "the reconciler composes nothing expensive"
// property true once the prompt axis stopped being pinned. Every reconcile
// trigger runs buildSessionOptions — a Settings save fanning out over every
// live Claude session, a thread-row config change, the deferred-restart
// watcher's re-check before it kills anything — and this render is the one
// step in that build that shells out (the git probe behind {{GIT_BLOCK}}).
//
// The repeat that actually matters is a live apply answering
// ErrLiveUpdateRequiresRestart: the render has already been paid, the update
// converged nothing, and the identical render is computed a second time when
// the watcher re-checks at the next quiet point. Memoized, that second pass
// is a three-field compare. (The watcher's BUSY polls cost nothing either
// way — threadConfigBusy short-circuits before any options are built.)
//
// ok is false when the render is over the size limit. That verdict is
// memoized with the same key as a successful render and surfaced to the
// thread once, because the caller's response to it is to keep running the
// PREVIOUS prompt: without a message, Settings would show one prompt and
// the session would be running another with nothing anywhere saying so
// (CLAUDE.md principle 5 — errors are user-facing state).
func (a *App) reconcilePromptOverrideRender(
	t store.Thread,
	sessionToken string,
	entry settings.PromptOverride,
	workDir string,
) (string, bool) {
	key := promptOverrideRender{source: entry.Prompt, workDir: workDir, model: t.Model}

	a.mu.Lock()
	cached, hit := a.promptOverrideRenders[sessionToken]
	a.mu.Unlock()
	if hit && cached.source == key.source && cached.workDir == key.workDir && cached.model == key.model {
		return cached.rendered, !cached.oversize
	}

	rendered := promptoverride.Render(entry.Prompt, a.promptOverrideFacts(t, entry.Prompt, workDir))
	oversize := len(rendered) > maxRenderedSystemPromptBytes
	if oversize {
		key.oversize = true
	} else {
		key.rendered = rendered
	}

	a.mu.Lock()
	if a.promptOverrideRenders == nil {
		a.promptOverrideRenders = make(map[string]promptOverrideRender)
	}
	a.promptOverrideRenders[sessionToken] = key
	a.mu.Unlock()

	if oversize {
		log.Printf("thread %s: system prompt override renders to %d bytes from a %d-byte prompt, over the %d-byte limit; keeping the live session's prompt",
			t.ID, len(rendered), len(entry.Prompt), maxRenderedSystemPromptBytes)
		// Host-synthesized, not wire-routed: this answers a Settings save,
		// not provider output, so it keeps the HandleSynthetic carve-out
		// that lets a stopped thread still receive it.
		a.emitErrorToThread(t.ID, fmt.Sprintf(
			"system prompt override not applied: it renders to %d bytes from a %d-byte prompt, over the %d-byte limit. "+
				"Check for a repeated {{GIT_BLOCK}} placeholder. This session keeps the prompt it started with, "+
				"and a new session on this thread will refuse to start until the override is smaller.",
			len(rendered), len(entry.Prompt), maxRenderedSystemPromptBytes))
		return "", false
	}
	return rendered, true
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
// Runs wherever an override is RENDERED — the spawn path, and the reconcile
// path when a live prompt swap re-rendered one — and takes the resolution
// its renderer already made rather than re-matching: a settings save landing
// between the two would otherwise let the mkdir act on a different entry
// than the one whose {{MEMORY_DIR}} was rendered into the prompt.
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

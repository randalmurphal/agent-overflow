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
	"agent-overflow/internal/stringsx"

	"github.com/shirou/gopsutil/v4/host"
)

// The settings-level system-prompt override and tool toggles
// (docs/specs/prompt-tool-overrides.md).
//
// Both axes are owned by Settings rather than by the thread row, which is
// what gives the feature its "next sessions only" semantics — and what
// makes the two entry points below a PAIR. applySettingsOwnedAxes stamps
// today's values on the spawn path; pinSettingsOwnedAxes carries the live
// session's forward on the reconcile path. Adding a third settings-owned
// axis means touching BOTH: an axis stamped only on the spawn side would
// queue a deferred restart on every live session the next time anything
// reconciled.

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

// promptOverrideResolution is the single decision a spawn makes about the
// settings-level override, so every downstream consumer reads the same
// answer from the same settings snapshot. Applied is false when a feature
// owns the prompt or no entry matched — in which case Entry is the zero
// value and nothing about the override is in play for this session.
type promptOverrideResolution struct {
	Entry   settings.PromptOverride
	Applied bool
}

// applySettingsOwnedAxes stamps the two axes Settings owns onto a freshly
// built options bundle: the system-prompt override (only when no feature
// already claimed the prompt) and the provider's disabled-tool list.
//
// SPAWN PATH ONLY — it renders placeholders, which costs a git probe, and
// its result is what the live session is then pinned to. The reconcile path
// calls pinSettingsOwnedAxes instead.
//
// One settings snapshot serves both axes and the returned resolution, so a
// save landing mid-spawn cannot produce a session whose prompt, tool list,
// and memory directory came from three different versions of the file.
//
// opts.WorkDir must already be final (design workdir resolved): a prompt
// that describes a directory the session is not in is worse than one that
// describes none.
func (a *App) applySettingsOwnedAxes(t store.Thread, opts *provider.SessionOptions) (promptOverrideResolution, error) {
	snapshot := a.settings.Get()

	// Provider-wide and settings-owned, so it is stamped here rather than
	// projected from the thread row — same reasoning as FastModeTierID.
	opts.DisabledTools = snapshot.DisabledToolsForProvider(t.Provider)
	opts.DisableTodoReminders = snapshot.TodoRemindersDisabledForProvider(t.Provider)

	// A non-empty prompt at this point means a FEATURE owns it (design
	// mode's artifact prompt, the discussion deliberation prompt — see
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
	return promptOverrideResolution{Entry: entry, Applied: true}, nil
}

// pinSettingsOwnedAxes carries the settings-owned axes of a LIVE session
// forward onto a freshly built options bundle, so the config reconciler
// diffs only what the thread row can change.
//
// RECONCILE PATH ONLY, and deliberately free of Match / Render / git — the
// reconciler runs under the per-thread config lock on every model, effort,
// or runtime-mode change, and composing an override there would pay for two
// git subprocesses to produce a value that is thrown away on the next line.
//
// The prompt pin is conditional because SystemPrompt is a shared axis:
// design mode and discussions own it for their threads, and an edit to the
// design prompt file must keep converging the way it always has. After
// buildSessionOptions, a non-empty prompt means exactly "a feature owns
// this" — no re-derivation needed.
func pinSettingsOwnedAxes(opts *provider.SessionOptions, launch provider.SessionOptions) {
	if opts.SystemPrompt == "" {
		opts.SystemPrompt = launch.SystemPrompt
	}
	opts.DisabledTools = launch.DisabledTools
	opts.DisableTodoReminders = launch.DisableTodoReminders
}

// featureOwnedSystemPrompt returns the prompt a FEATURE owns for this
// thread: design mode's artifact prompt and the discussion deliberation
// prompt. Non-empty means the settings-level override is skipped — those
// features already run a fully custom prompt and replacing it would break
// them.
//
// The one definition of that precedence rule, and it is evaluated exactly
// once per build (in buildSessionOptions). Everything downstream reads the
// answer off opts.SystemPrompt instead: after the build, non-empty means
// "a feature owns this" and empty means "the settings override may claim
// it". Re-deriving it later is how the two sides drift.
func (a *App) featureOwnedSystemPrompt(t store.Thread, designCfg designSessionConfig) string {
	return stringsx.JoinNonEmpty("\n\n", designCfg.Prompt, a.threadSystemPrompt(t.ID))
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
// The home is os.UserHomeDir() because that is what the spawned CLI
// itself resolves: the headless spawn clears CLAUDE_CONFIG_DIR outright
// and both Claude providers refuse it from a user's custom environment
// (provider.ReservedEnvNames), so `~/.claude` under $HOME is the home the
// session will read its memory from.
func (a *App) claudeMemoryDirForThread(t store.Thread, workDir string) (string, bool, error) {
	if t.Provider != string(provider.Claude) && t.Provider != string(provider.ClaudeTUI) {
		return "", false, nil
	}
	if strings.TrimSpace(workDir) == "" {
		return "", false, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", false, fmt.Errorf("resolve home directory: %w", err)
	}
	return promptoverride.ClaudeMemoryDir(home, workDir)
}

// ensureClaudeMemoryDir creates the memory directory a rendered override
// promised the model already exists. Under a replaced system prompt the CLI
// stops creating it (verified 2.1.234) while memory RECALL keeps working,
// so this is what makes the write side of the feature real.
//
// Runs on the spawn path only, and takes the resolution applySettingsOwnedAxes
// already made rather than re-matching: a settings save landing between the
// two would otherwise let the mkdir act on a different entry than the one
// whose {{MEMORY_DIR}} was rendered into the prompt.
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

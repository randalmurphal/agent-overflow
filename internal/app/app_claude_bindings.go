package app

import (
	"agent-overflow/internal/claudeapp"
	"agent-overflow/internal/claudeconfig"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/slicesx"
)

// ClaudeSlashCommands is the wire shape of GetClaudeSlashCommands.
//
// Probed is the field that carries the nil-vs-empty distinction a JSON array
// cannot: false means NO PROBE HAS ANSWERED for the active Claude identity, so
// the list is unknown rather than empty, and a command menu must not render it
// as "this binary has none". Commands is always an array on the wire so the
// two facts stay on separate fields instead of overloading `null`.
type ClaudeSlashCommands struct {
	Probed   bool                    `json:"probed"`
	Commands []provider.SlashCommand `json:"commands"`
}

// ThreadContextUsage is the answer to "what is actually in this thread's
// context window right now".
//
// Available=false is a first-class answer, not a failure: the breakdown
// exists only while a Claude process is running, and there is no honest way
// to synthesise it from history. Callers render Reason, never zeros.
// Genuine faults (a wedged CLI, a provider-side error) come back as a Go
// error instead, so "no session" and "something broke" never collapse into
// the same UI state.
type ThreadContextUsage struct {
	Available bool `json:"available"`
	// Reason is a short user-facing sentence, set only when Available is
	// false.
	Reason string `json:"reason,omitempty"`
	// TotalTokens is the context the model actually sees. Deferred tool
	// definitions are listed in Categories but excluded from this figure.
	TotalTokens int `json:"totalTokens"`
	// MaxTokens is the model's context window as the CLI reports it.
	MaxTokens int `json:"maxTokens"`
	// Percentage is the CLI's own rounded occupancy. Displayed as given —
	// the CLI owns the denominator it used.
	Percentage int `json:"percentage"`
	// Model is the slug the breakdown was computed for. It can legitimately
	// differ from the thread's configured model when a live model switch is
	// pending (set_model applies from the next turn).
	Model string `json:"model,omitempty"`
	// Categories is the breakdown in the CLI's own order. Never nil when
	// Available is true.
	Categories []ThreadContextUsageCategory `json:"categories"`
}

// ThreadContextUsageCategory is one row of the breakdown. Name is passed
// through from the CLI verbatim so a category added in a future release
// still renders.
type ThreadContextUsageCategory struct {
	Name   string `json:"name"`
	Tokens int    `json:"tokens"`
	// Deferred rows are excluded from TotalTokens. A consumer that sums the
	// rows must skip them or it will overcount.
	Deferred bool `json:"deferred,omitempty"`
}

// GetClaudeSlashCommands returns the provider-executed slash commands the last
// zero-token account probe reported for the active Claude identity, with their
// descriptions and argument hints.
//
// This is the RICH half of a Claude thread's command menu and it is available
// before any session exists — the probe runs at startup, so a composer can seed
// its menu on a cold thread. It is NOT the whole answer: the probe answers for a
// probe identity, not for this thread's session, and only the live per-thread
// `provider:commands` frames carry MCP prompt commands (`mcp__server__prompt`).
// The frontend unions the two; deliberately not unioned here, because this
// method has no thread and triage may not reach into a provider-specific cache
// (see internal/triage/provider_commands.go).
//
// Never spawns — a pure read of what a probe already left behind.
//
//ao:scope threads:read
func (a *App) GetClaudeSlashCommands() ClaudeSlashCommands {
	commands, probed := a.providerDiscoveryService().ClaudeCommands()
	return ClaudeSlashCommands{Probed: probed, Commands: slicesx.OrEmpty(commands)}
}

// GetThreadContextUsage returns Claude's canonical `/context` breakdown for
// a thread's live session.
//
// This is a user-initiated, live-session-only read. It is not cached, not
// persisted, and not polled: the numbers describe the provider process's
// state right now and are stale as soon as the next turn runs (Core
// Principle 2 — the provider process is the source of truth during a turn,
// and we do not duplicate its state). The passive `message_delta.usage`
// signal continues to drive the always-on meter; this is the exact reading
// behind it.
//
// Local-only on the wire: it drives a provider session running under the
// user's credentials on the host.
//
//ao:scope threads:operate
func (a *App) GetThreadContextUsage(threadID string) (ThreadContextUsage, error) {
	if a.shuttingDown.Load() {
		return ThreadContextUsage{}, ErrShuttingDown
	}
	result, err := a.claudeAppService().GetContextUsage(threadID)
	if err != nil {
		return ThreadContextUsage{}, err
	}
	if result.Usage == nil {
		return ThreadContextUsage{Reason: result.Reason}, nil
	}
	return projectThreadContextUsage(result.Usage), nil
}

func projectThreadContextUsage(usage *claude.ContextUsage) ThreadContextUsage {
	out := ThreadContextUsage{
		Available:   true,
		TotalTokens: usage.TotalTokens,
		MaxTokens:   usage.MaxTokens,
		Percentage:  usage.Percentage,
		Model:       usage.Model,
		Categories:  make([]ThreadContextUsageCategory, 0, len(usage.Categories)),
	}
	for _, category := range usage.Categories {
		out.Categories = append(out.Categories, ThreadContextUsageCategory{
			Name: category.Name, Tokens: category.Tokens, Deferred: category.Deferred,
		})
	}
	return out
}

// GetClaudeSkills enumerates the Claude skills a session started in
// workspacePath would load — user tier (~/.claude/skills), project tier
// (<workspace>/.claude/skills), and enabled plugins' skills — straight
// from the filesystem, without spawning anything.
//
// This exists because the zero-token account probe runs --safe-mode,
// whose initialize response deliberately omits skills: without this
// read a cold thread's composer menu cannot list them until a session's
// `system/init` frame arrives. The frontend unions this list under the
// same rule as the probe commands — a live session's name set stays
// authoritative once it exists, and this list only fills in before that
// or enriches names with descriptions.
//
// workspacePath must be ABSOLUTE, like GetCodexSkills: skills are
// directory-scoped and a relative path would silently resolve against
// the app's own cwd.
//
// LocalOnly on the wire: it reads the user's home directory and the
// workspace tree, and its rows name what is installed on the host.
//
//ao:scope threads:operate
func (a *App) GetClaudeSkills(workspacePath string) ([]claudeconfig.Skill, error) {
	return a.claudeAppService().Skills(workspacePath)
}

// StopClaudeTask asks the Claude CLI to kill a backgrounded task
// (run_in_background Bash or a Task subagent) identified by `taskID`.
// On success the CLI emits a follow-up `system/task_updated` with
// `patch.status:"killed"` on the normal event stream, which flows
// through triage into the sibling `tool_completion` row as
// status=killed — rendered as a distinct "Stopped" badge in the UI.
//
// Returns typed errors for:
//
//   - session-missing: no Claude session for this thread. The caller
//     started a stop before Start / after Close.
//   - provider-mismatch: the thread exists but it's a Codex session,
//     not a Claude one. Codex's per-row stop is a different RPC with a
//     different id namespace (process id, not task id) — see
//     codex.Session.TerminateBackgroundTerminal and
//     docs/references/codex.md#background-terminals; the frontend must
//     branch on provider before reaching for this.
//   - timeout / provider error: surfaced verbatim so the UI can render
//     the CLI-supplied message.
//
//ao:scope threads:operate
func (a *App) StopClaudeTask(threadID, taskID string) error {
	if a.shuttingDown.Load() {
		return ErrShuttingDown
	}
	return a.claudeAppService().StopTask(threadID, taskID)
}

// BackgroundClaudeTask moves an in-flight FOREGROUND Claude task (a
// running subagent or a foreground Bash) to the background, identified
// by the `toolUseID` of the block that started it — the control-request
// form of the Claude TUI's Ctrl+B, and the wire behind the background
// button on a running agent / Bash row.
//
// Keyed by tool_use_id, NOT task_id, because that is what the CLI's
// `background_tasks` subtype takes and because the UI's button lives on
// the launch row, whose id IS the tool_use_id. StopClaudeTask is the
// sibling in the opposite direction and takes a task_id — the two ids
// are not interchangeable, which is why the bindings do not share one
// parameter name.
//
// On success the CLI answers `{backgrounded:true}` and then emits
// `system/task_updated {patch:{is_backgrounded:true}}`, which flows
// through triage as EventSubagentBackgrounded and stamps the launch row
// with the moment its sidechain streaming stopped.
//
// Returns typed errors for:
//
//   - session-missing: no Claude session for this thread. The caller
//     backgrounded before Start / after Close.
//   - provider-mismatch: the thread exists but it's a Codex session.
//     Codex has no equivalent — a spawned collab-agent child is already
//     asynchronous and `close_agent` is a model-only tool — so the
//     frontend must branch on provider before reaching for this.
//   - timeout / provider error: surfaced verbatim, including the CLI's
//     refusal when no foreground task matched the tool_use_id.
//
//ao:scope threads:operate
func (a *App) BackgroundClaudeTask(threadID, toolUseID string) error {
	if a.shuttingDown.Load() {
		return ErrShuttingDown
	}
	return a.claudeAppService().BackgroundTask(threadID, toolUseID)
}

func (a *App) claudeAppService() *claudeapp.Service {
	a.claudeAppOnce.Do(func() {
		a.claudeApp = claudeapp.New(claudeapp.Deps{
			Session: func(threadID string) (*claude.Session, bool) {
				session, active := a.sessionManager().get(threadID)
				return session.Claude, active
			},
			ConfigStore: a.claudeConfig,
		})
	})
	return a.claudeApp
}

func (a *App) claudeModelsForProvider(providerName string) []provider.ModelInfo {
	return a.providerDiscoveryService().ClaudeModels(providerName)
}

func (a *App) claudeProbeModelKey() provider.ProbeCacheKey {
	return a.providerDiscoveryService().ClaudeProbeKey()
}

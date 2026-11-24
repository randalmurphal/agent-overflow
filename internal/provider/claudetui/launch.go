package claudetui

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/stringsx"
	"agent-overflow/internal/terminal"
)

// launch.go assembles the PTY launch for the interactive claude: the
// full-access flag set, the --settings hook registration that points Claude at
// the AO relay subcommand, the two settings-owned override flags
// (--system-prompt-file / --disallowedTools, both spike-verified to work
// interactively on 2.1.234 — see docs/specs/prompt-tool-overrides.md), the
// app guide's --append-system-prompt-file and the version-gated
// --system-prompt-snapshot off (both spike-verified on 2.1.284), and the
// env that injects the gateway base URL and the per-session relay url +
// capability token.
//
// Clean-launch pre-seeds (hasTrustDialogAccepted / bypassPermissionsModeAccepted)
// are NOT written here — they live in the user's real config (present for any
// full-access user) and a stray acceptance dialog is caught by the stall
// detector → take-control escape hatch rather than by mutating the user's
// config files. The org killswitch (disableBypassPermissionsMode) surfaces as a
// launch failure the session reports as user-facing state.

// buildLaunchOptions produces the terminal.SessionOptions that spawn the
// interactive claude under the session's gateway + relay.
//
// systemPromptPath and appendSystemPromptPath are the files
// claude.WriteSystemPromptFile produced for cfg.SystemPrompt and
// cfg.AppendSystemPrompt (empty for the one the session does not carry) — a
// prompt reaches the CLI by path, never as an argv value, so this function
// never reads either text itself. Same split as the headless buildArgs.
func buildLaunchOptions(cfg Config, systemPromptPath, appendSystemPromptPath, gatewayURL, hookURL, hookToken string) (terminal.SessionOptions, error) {
	if cfg.Binary == "" {
		return terminal.SessionOptions{}, fmt.Errorf("claudetui: no claude binary configured")
	}
	if cfg.WorkDir == "" {
		return terminal.SessionOptions{}, fmt.Errorf("claudetui: no workdir configured")
	}

	settings, err := hookSettingsJSON(cfg)
	if err != nil {
		return terminal.SessionOptions{}, err
	}

	args := []string{
		"--permission-mode", "bypassPermissions",
		"--allow-dangerously-skip-permissions",
		// Opt thinking text onto the wire. Opus 4.7+ (including the opus-4-8 the
		// interactive TUI runs) default the API `thinking.display` to `omitted`:
		// the thinking block is still emitted with a signature, but its `thinking`
		// string is EMPTY and no `thinking_delta` events fire — so the gateway has
		// nothing to reconstruct and neither AO nor the TUI itself shows any
		// thinking. The flag is global and the interactive TUI honors it on the
		// wire: LIVE-confirmed on 2.1.170 in
		// the CLI capture spike thinking_title — the request carried
		// thinking.display:"summarized" and the response streamed thinking_delta
		// text (124 chars on a math turn) where the omitted control run streamed
		// none. Mirrors the headless path (provider/claude/session.go) and is a
		// no-op on models that already default to summarized. See
		// docs/references/claude-wire.md §"Extended thinking".
		"--thinking-display", "summarized",
	}
	if cfg.Model != "" {
		args = append(args, "--model", cfg.Model)
	}
	if cfg.ReasoningEffort != "" {
		// Same global --effort flag headless passes (provider/claude/session.go);
		// without it the interactive TUI runs at the model's default tier and the
		// AO effort selection is silently ignored.
		args = append(args, "--effort", cfg.ReasoningEffort)
	}
	if cfg.Resume != "" {
		args = append(args, "--resume", cfg.Resume)
	}
	for _, dir := range cfg.AdditionalDirs {
		// One flag per directory, exactly as the headless buildArgs emits
		// it; the CLI's variadic option concatenates repeated occurrences.
		args = append(args, "--add-dir", dir)
	}
	if systemPromptPath != "" {
		// The settings-level system-prompt override
		// (docs/specs/prompt-tool-overrides.md). The interactive TUI honors
		// the same flag headless does — the request's `system` array becomes
		// [billing header, the TUI's fixed identity line "You are Claude
		// Code, Anthropic's official CLI for Claude.", the file's content],
		// i.e. full body replacement (spike-verified 2.1.234 via PTY + wire
		// capture; see docs/references/claude-wire.md §"System prompt
		// assembly"). The prompt itself never reaches argv — see
		// claude.WriteSystemPromptFile.
		args = append(args, "--system-prompt-file", systemPromptPath)
	}
	if appendSystemPromptPath != "" {
		// The app guide, after whichever prompt the session otherwise runs
		// with; the TUI composes `--system-prompt-file` and this flag exactly
		// as headless does (2.1.284 PTY + wire capture).
		args = append(args, "--append-system-prompt-file", appendSystemPromptPath)
	}
	// The TUI snapshots the first request's prompt into the transcript and
	// reuses it on resume exactly as headless does (same capture), so the
	// same version-gated opt-out applies.
	args = append(args, claude.SystemPromptSnapshotArgs(cfg.InstalledCLIVersion)...)
	for _, tool := range cfg.DisallowedTools {
		// One flag per name; the named tools' schemas are absent from the
		// TUI's requests (same 2.1.234 capture). Worth knowing when reading a
		// user's list: the CLI aliases Task and Agent, so disallowing "Task"
		// removes the "Agent" tool too.
		//
		// The field's contract is "already through
		// claude.SanitizeDisallowedTools" (ConfigFromOptions runs it), which
		// is what makes appending each name verbatim safe here — same split
		// as the headless buildArgs.
		args = append(args, "--disallowedTools", tool)
	}

	if mcpJSON, ok := claude.MCPConfigJSON(cfg.MCPServers); ok {
		// The same flag and payload as the headless buildArgs; one binary
		// reads it the same way in both modes.
		args = append(args, "--mcp-config", mcpJSON)
	}
	for _, tool := range claude.SanitizeAllowedTools(cfg.AllowedTools) {
		args = append(args, "--allowedTools", tool)
	}

	// The hook block and cfg.Retention's cleanupPeriodDays share the one
	// `--settings` flag; the transcript sweep covers the whole config home,
	// whichever surface runs it (claude/retention.go).
	line, err := claude.NewCommandLine(cfg.Retention, settings, args...)
	if err != nil {
		return terminal.SessionOptions{}, fmt.Errorf("claudetui: %w", err)
	}
	return terminal.SessionOptions{
		Shell:  cfg.Binary,
		Args:   line.Args(),
		Cwd:    cfg.WorkDir,
		Env:    buildEnv(line, cfg, gatewayURL, hookURL, hookToken),
		Rows:   cfg.rows(),
		Cols:   cfg.cols(),
		Reaper: cfg.Reaper,
	}, nil
}

// hookEvents are the events the relay command is registered for. PreToolUse is
// matched to AskUserQuestion only; the rest are observe-mode (whole-event).
var observeHookEvents = []string{
	"PostToolUse",
	"PostToolUseFailure",
	"SessionStart",
	"PreCompact",
	"PostCompact",
}

// observedHooks is a copy of the hook groups hooks holds for event.
func observedHooks(hooks map[string]any, event string) []any {
	groups, _ := hooks[event].([]any)
	return append([]any{}, groups...)
}

// hookSettingsJSON renders the --settings flag payload, the flagSettings
// layer, which merges with (does not clobber) the user's own settings and
// hooks. It registers the AO relay subcommand for every captured event, and
// mirrors the parts of the headless block the app owns on every Claude
// surface: the cross-session inbound policy, the native-memory switch, the
// memory SessionStart and UserPromptSubmit hooks and the additional
// directories
// (docs/specs/memory.md).
//
// crossSessionInbound is here for the reason the headless block states it
// (claude/options.go §claudeCrossSessionInbound): the peer registry is
// machine-wide, AO's env gate is not the only thing that can bind the inbox,
// and an ABSENT key is the CLI's mode-parity path, which, with the two remote
// GrowthBook flags live, auto-delivers a peer's message as a user turn in a
// session whose user never opted in. "Off means off" is not a headless-only
// property, so the refusal is stated on this surface too.
//
// The rest of the headless block (fastMode, outputStyle, the env map) is
// deliberately not mirrored: those axes are spawn options the app resolves for
// a headless session, and the interactive TUI is the user's own CLI, which
// keeps its own settings-file resolution for them.
func hookSettingsJSON(cfg Config) (string, error) {
	cmd, err := hookCommand(cfg)
	if err != nil {
		return "", err
	}
	observeEntry := []any{map[string]any{
		"hooks": []any{map[string]any{"type": "command", "command": cmd}},
	}}

	hooks := map[string]any{
		// AskUserQuestion is the one blocking hook; a generous timeout gives
		// the human a real answer window (the spike confirmed no clamp).
		"PreToolUse": []any{map[string]any{
			"matcher": "AskUserQuestion",
			"hooks":   []any{map[string]any{"type": "command", "command": cmd, "timeout": 600}},
		}},
	}
	for _, ev := range observeHookEvents {
		hooks[ev] = observeEntry
	}
	// The memory hook joins whatever observes the same event.
	if hook := cfg.SessionStartHook; hook != nil {
		hooks["SessionStart"] = append(observedHooks(hooks, "SessionStart"), claude.SessionStartHookGroup(*hook))
		if prompt := claude.UserPromptSubmitHookGroup(*hook); prompt != nil {
			hooks["UserPromptSubmit"] = append(observedHooks(hooks, "UserPromptSubmit"), prompt)
		}
	}

	block := map[string]any{"hooks": hooks}
	if inbound := strings.TrimSpace(cfg.CrossSessionInbound); inbound != "" {
		block["crossSessionInbound"] = inbound
	}
	if cfg.NativeMemoryOff {
		block["autoMemoryEnabled"] = false
		block["env"] = claude.NativeMemoryOffEnv(true)
	}
	if permissions := claude.AccessPermissions(cfg.AccessDirs); permissions != nil {
		block["permissions"] = permissions
	}

	data, err := json.Marshal(block)
	if err != nil {
		return "", fmt.Errorf("claudetui: marshal hook settings: %w", err)
	}
	return string(data), nil
}

// hookCommand is the shell command Claude runs for each hook: the AO executable
// invoked with the __claude-hook subcommand. The path is shell-quoted so a
// space in the install path doesn't split the command.
func hookCommand(cfg Config) (string, error) {
	exe := cfg.HookCmd
	if exe == "" {
		var err error
		exe, err = os.Executable()
		if err != nil {
			return "", fmt.Errorf("claudetui: resolve AO executable for hook: %w", err)
		}
	}
	return stringsx.ShellQuote(exe) + " " + HookSubcommand, nil
}

// The child's ANTHROPIC_BASE_URL (provider.ClaudeBaseURLEnv) is this
// provider's: it must resolve to the per-session loopback gateway, so a
// caller with a user-configured endpoint passes it as Config.Upstream
// instead, and the gateway forwards there.

// buildEnv adds the session's own variables to cfg.Env: the gateway and
// relay pins, the command line's clears and pins (the config home every
// Claude launch uses; the peer-inbox gate and peer-visible name, since
// CLAUDE_CODE_HARBOR_KITE is a parsed boolean the CLI reads before any
// settings layer; and the task-list key, the thread's identity), and the
// todo defaults headless Claude applies (claude.WithTodoDefaults).
func buildEnv(line claude.CommandLine, cfg Config, gatewayURL, hookURL, hookToken string) []string {
	in := line.Env(cfg.Env).
		Pin(provider.ClaudeBaseURLEnv, gatewayURL).
		Pin(provider.AOEnv(envHookURL), hookURL).
		Pin(provider.AOEnv(envHookToken), hookToken)
	if cfg.CrossSessionEnabled {
		in = in.Pin(provider.ClaudeCrossSessionGateEnv, "1")
	}
	if cfg.TaskListID != "" {
		in = in.Pin(provider.ClaudeTaskListIDEnv, cfg.TaskListID)
	}
	return provider.BuildEnvironment(claude.WithTodoDefaults(in, cfg.DisableTodoReminders))
}

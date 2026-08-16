package claudetui

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"agent-overflow/internal/terminal"
)

// launch.go assembles the PTY launch for the interactive claude: the
// full-access flag set, the --settings hook registration that points Claude at
// the AO relay subcommand, and the env that injects the gateway base URL and
// the per-session relay url + capability token.
//
// Clean-launch pre-seeds (hasTrustDialogAccepted / bypassPermissionsModeAccepted)
// are NOT written here — they live in the user's real config (present for any
// full-access user) and a stray acceptance dialog is caught by the stall
// detector → take-control escape hatch rather than by mutating the user's
// config files. The org killswitch (disableBypassPermissionsMode) surfaces as a
// launch failure the session reports as user-facing state.

// buildLaunchOptions produces the terminal.SessionOptions that spawn the
// interactive claude under the session's gateway + relay.
func buildLaunchOptions(cfg Config, gatewayURL, hookURL, hookToken string) (terminal.SessionOptions, error) {
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
		"--settings", settings,
		// Opt thinking text onto the wire. Opus 4.7+ (including the opus-4-8 the
		// interactive TUI runs) default the API `thinking.display` to `omitted`:
		// the thinking block is still emitted with a signature, but its `thinking`
		// string is EMPTY and no `thinking_delta` events fire — so the gateway has
		// nothing to reconstruct and neither AO nor the TUI itself shows any
		// thinking. The flag is global and the interactive TUI honors it on the
		// wire: LIVE-confirmed on 2.1.170 in
		// spike/claude-mitm/probe_thinking_title.py — the request carried
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

	return terminal.SessionOptions{
		Shell: cfg.Binary,
		Args:  args,
		Cwd:   cfg.WorkDir,
		Env:   buildEnv(cfg.Env, gatewayURL, hookURL, hookToken),
		Rows:  cfg.rows(),
		Cols:  cfg.cols(),
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

// hookSettingsJSON renders the --settings flag payload registering the AO relay
// subcommand for every captured event. The flag is the flagSettings layer,
// which merges with (does not clobber) the user's own hooks.
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

	data, err := json.Marshal(map[string]any{"hooks": hooks})
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
	return shellQuote(exe) + " " + HookSubcommand, nil
}

// BaseURLEnv is the variable that points Claude Code at an API endpoint. This
// provider OWNS the child's copy — it must resolve to the per-session loopback
// gateway — so a caller with a user-configured endpoint passes it as
// Config.Upstream instead, and the gateway forwards there. Exported so the app
// layer names the same variable this package strips.
const BaseURLEnv = "ANTHROPIC_BASE_URL"

// todoToolsEnvVar opts the TUI session into the TodoWrite / Task* tool
// surface that claude ≥2.1.233 removes for modern models. Same rationale
// and same posture as the headless provider's claudeTodoToolsEnvVar
// (internal/provider/claude/session.go): AO's activity-rail todo list
// rides those tools' events, so the session defaults in — but as a
// DEFAULT, not an owned key. A user value in the custom environment
// (which lands in Config.Env before buildEnv runs) survives, so setting
// it to "false" restores the vendor's stock TUI tool surface.
const todoToolsEnvVar = "CLAUDE_CODE_ENABLE_TODO_TOOLS"

// buildEnv layers the per-session gateway + relay env onto the base
// environment, stripping any inherited values for the keys we own so a dirty
// parent env can't redirect Claude away from our gateway, and fills in the
// todo-tools default when the base carries no value of its own.
func buildEnv(base []string, gatewayURL, hookURL, hookToken string) []string {
	if len(base) == 0 {
		// Honor the documented "empty means inherit" Config.Env contract for an
		// empty-but-non-nil slice too, not just nil.
		base = os.Environ()
	}
	owned := map[string]struct{}{
		BaseURLEnv:   {},
		envHookURL:   {},
		envHookToken: {},
	}
	out := make([]string, 0, len(base)+4)
	haveTodoOptIn := false
	for _, kv := range base {
		if key, _, ok := strings.Cut(kv, "="); ok {
			if _, isOwned := owned[key]; isOwned {
				continue
			}
			if key == todoToolsEnvVar {
				haveTodoOptIn = true
			}
		}
		out = append(out, kv)
	}
	out = append(out,
		BaseURLEnv+"="+gatewayURL,
		envHookURL+"="+hookURL,
		envHookToken+"="+hookToken,
	)
	if !haveTodoOptIn {
		out = append(out, todoToolsEnvVar+"=true")
	}
	return out
}

// shellQuote wraps s in single quotes, escaping any embedded single quotes,
// so it survives Claude Code running the hook command through a shell.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

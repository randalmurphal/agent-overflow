package claude

import (
	"fmt"

	"agent-overflow/internal/provider"
)

func claudeEffortFromOption(effort provider.ReasoningEffort) string {
	switch effort {
	case provider.EffortLow:
		return "low"
	case provider.EffortMedium:
		return "medium"
	case provider.EffortHigh:
		return "high"
	case provider.EffortXHigh:
		// Claude CLI accepts max for the extra-high tier on models whose SDK
		// descriptor names the option xhigh.
		return "max"
	case provider.EffortMax:
		return "max"
	default:
		return ""
	}
}

func envForContextWindowAndAutoCompact(contextWindow, autoCompactPercent int) map[string]string {
	var env map[string]string
	if autoCompactPercent <= 0 {
		return env
	}
	if autoCompactPercent > 90 {
		autoCompactPercent = 90
	}
	if env == nil {
		env = map[string]string{}
	}
	env["CLAUDE_AUTOCOMPACT_PCT_OVERRIDE"] = fmt.Sprintf("%d", autoCompactPercent)
	return env
}

func claudeModelForContextWindow(model string, contextWindow int) string {
	if model != "" && contextWindow == provider.ClaudeExtendedContextWindow {
		return model + "[1m]"
	}
	return model
}

func claudeFastMode(model string, fastMode bool) bool {
	return fastMode && provider.ModelSupportsCapability(string(provider.Claude), model, provider.ModelCapabilityFastMode)
}

// ConfigFromOptions translates a provider-agnostic SessionOptions bundle into
// the Claude-specific launch Config. This is the single place where the
// effort flag, 1M-context model suffix, permission flags, and fast-mode
// settings are applied. Callers in app.go pass the result straight into
// claude.NewSession plus any ancillary wiring (Binary, EventLogger) that
// lives outside the session-options abstraction.
func ConfigFromOptions(opts provider.SessionOptions) Config {
	model := opts.Model
	contextWindow := provider.ResolveContextWindowForModel(string(provider.Claude), model, opts.ContextWindow)
	autoCompactPercent := provider.AutoCompactPercentForContextTier(
		provider.ContextTierForModelWindow(string(provider.Claude), model, contextWindow),
		opts.AutoCompactStandardPercent,
		opts.AutoCompactExtendedPercent,
	)
	if autoCompactPercent == 0 {
		autoCompactPercent = opts.AutoCompactPercent
	}

	return Config{
		Model:              claudeModelForContextWindow(model, contextWindow),
		WorkDir:            opts.WorkDir,
		Resume:             opts.Resume,
		ForkSession:        opts.ForkSession,
		SystemPrompt:       opts.SystemPrompt,
		ReasoningEffort:    claudeEffortFromOption(opts.ReasoningEffort),
		FastMode:           claudeFastMode(model, opts.FastMode),
		PermissionFlags:    claudePermissionFlags(opts.RuntimeMode),
		BasePermissionMode: claudeBasePermissionMode(opts.RuntimeMode),
		InteractionMode:    opts.Mode,
		Env:                envForContextWindowAndAutoCompact(contextWindow, autoCompactPercent),
	}
}

// claudePermissionFlags maps a RuntimeMode to the raw CLI flag sequence the
// Claude SDK would send to headless `claude -p`.
func claudePermissionFlags(mode provider.RuntimeMode) []string {
	permissionMode := claudeBasePermissionMode(mode)
	if permissionMode == "default" {
		return nil
	}
	flags := []string{"--permission-mode", permissionMode}
	if permissionMode == "bypassPermissions" {
		flags = append(flags, "--allow-dangerously-skip-permissions")
	}
	return flags
}

// claudeBasePermissionMode maps a RuntimeMode to Claude's permission-mode
// value. Unlike claudePermissionFlags, it returns "default" for supervised
// mode so sessions can restore the base mode via set_permission_mode after a
// plan turn.
func claudeBasePermissionMode(mode provider.RuntimeMode) string {
	switch mode {
	case provider.RuntimeAutoAcceptEdits:
		return "acceptEdits"
	case provider.RuntimeFullAccess:
		return "bypassPermissions"
	case provider.RuntimeApprovalRequired:
		fallthrough
	default:
		return "default"
	}
}

package settings

import (
	"fmt"
	"log"
	"strings"
	"unicode"
)

var (
	allowedThemes = map[string]struct{}{
		"system": {},
		"light":  {},
		"dark":   {},
	}
	allowedTimestampFormats = map[string]struct{}{
		"locale":  {},
		"12-hour": {},
		"24-hour": {},
	}
	// allowedTextGenerationProviders enumerates the commit-message /
	// text-generation backend. Mirrors t3-code's RoutingTextGeneration —
	// only the Claude and Codex CLIs have the structured-output flags
	// we rely on.
	allowedTextGenerationProviders = map[string]struct{}{
		"claude": {},
		"codex":  {},
	}
	// allowedReasoningEfforts mirrors provider.AllReasoningEfforts — duplicated
	// here to keep internal/settings dependency-free of the provider package.
	allowedReasoningEfforts = map[string]struct{}{
		"low":    {},
		"medium": {},
		"high":   {},
		"xhigh":  {},
		"max":    {},
	}
	allowedThreadEnvModes = map[string]struct{}{
		"local":    {},
		"worktree": {},
	}
)

func validateSettings(current Settings) (Settings, error) {
	current.Theme = strings.TrimSpace(current.Theme)
	if err := validateOption("theme", current.Theme, allowedThemes); err != nil {
		return Settings{}, err
	}

	current.TimestampFormat = strings.TrimSpace(current.TimestampFormat)
	if err := validateOption("timestampFormat", current.TimestampFormat, allowedTimestampFormats); err != nil {
		return Settings{}, err
	}

	current.ClaudeBinaryPath = normalizeBinaryPath(current.ClaudeBinaryPath, DefaultSettings.ClaudeBinaryPath)
	current.CodexBinaryPath = normalizeBinaryPath(current.CodexBinaryPath, DefaultSettings.CodexBinaryPath)
	current.RecentWorkspaces = normalizeRecentWorkspaces(current.RecentWorkspaces)
	current.ObservabilityOtlpEndpoint = strings.TrimSpace(current.ObservabilityOtlpEndpoint)

	current.DefaultThreadEnvMode = strings.TrimSpace(current.DefaultThreadEnvMode)
	if err := validateOption(
		"defaultThreadEnvMode",
		current.DefaultThreadEnvMode,
		allowedThreadEnvModes,
	); err != nil {
		return Settings{}, err
	}

	var err error
	current.WorktreeBranchPrefix, err = validateWorktreeBranchPrefix(current.WorktreeBranchPrefix)
	if err != nil {
		return Settings{}, err
	}

	current.TextGenerationProvider = strings.TrimSpace(current.TextGenerationProvider)
	if err := validateOption(
		"textGenerationProvider",
		current.TextGenerationProvider,
		allowedTextGenerationProviders,
	); err != nil {
		return Settings{}, err
	}

	// TextGenerationModel is optional ("" == use per-provider default).
	// Trim but don't reject empty.
	current.TextGenerationModel = strings.TrimSpace(current.TextGenerationModel)

	current.TextGenerationReasoningEffort = strings.TrimSpace(current.TextGenerationReasoningEffort)
	if err := validateOption(
		"textGenerationReasoningEffort",
		current.TextGenerationReasoningEffort,
		allowedReasoningEfforts,
	); err != nil {
		return Settings{}, err
	}

	// Editor.Preference is open-vocabulary because detection mints
	// dynamic IDs (catalog editors plus "env:editor" / "env:visual"
	// fallbacks). We trim but don't enum-validate — an unknown value
	// just falls through to the catalog priority at resolve time, no
	// schema migration required when a new editor lands.
	current.Editor.Preference = strings.TrimSpace(current.Editor.Preference)
	return current, nil
}

func sanitizeLoadedSettings(current Settings) Settings {
	current.Theme = sanitizeOption("theme", current.Theme, DefaultSettings.Theme, allowedThemes)
	current.TimestampFormat = sanitizeOption(
		"timestampFormat",
		current.TimestampFormat,
		DefaultSettings.TimestampFormat,
		allowedTimestampFormats,
	)
	current.ClaudeBinaryPath = sanitizeBinaryPath(
		"claudeBinaryPath",
		current.ClaudeBinaryPath,
		DefaultSettings.ClaudeBinaryPath,
	)
	current.CodexBinaryPath = sanitizeBinaryPath(
		"codexBinaryPath",
		current.CodexBinaryPath,
		DefaultSettings.CodexBinaryPath,
	)
	current.RecentWorkspaces = normalizeRecentWorkspaces(current.RecentWorkspaces)
	current.ObservabilityOtlpEndpoint = strings.TrimSpace(current.ObservabilityOtlpEndpoint)

	current.DefaultThreadEnvMode = sanitizeOption(
		"defaultThreadEnvMode",
		current.DefaultThreadEnvMode,
		DefaultSettings.DefaultThreadEnvMode,
		allowedThreadEnvModes,
	)
	current.WorktreeBranchPrefix = sanitizeWorktreeBranchPrefix(current.WorktreeBranchPrefix)

	current.TextGenerationProvider = sanitizeOption(
		"textGenerationProvider",
		current.TextGenerationProvider,
		DefaultSettings.TextGenerationProvider,
		allowedTextGenerationProviders,
	)
	// TextGenerationModel is optional — normalize whitespace but keep "" as
	// a legal value meaning "use the per-provider default".
	current.TextGenerationModel = strings.TrimSpace(current.TextGenerationModel)
	current.TextGenerationReasoningEffort = sanitizeOption(
		"textGenerationReasoningEffort",
		current.TextGenerationReasoningEffort,
		DefaultSettings.TextGenerationReasoningEffort,
		allowedReasoningEfforts,
	)
	current.Editor.Preference = strings.TrimSpace(current.Editor.Preference)
	return current
}

func validateOption(field, value string, allowed map[string]struct{}) error {
	if _, ok := allowed[value]; !ok {
		return fmt.Errorf("%s must be one of %s", field, joinAllowedValues(allowed))
	}
	return nil
}

func validateWorktreeBranchPrefix(value string) (string, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "", fmt.Errorf("worktreeBranchPrefix cannot be empty")
	}
	if strings.HasPrefix(trimmed, "-") {
		return "", fmt.Errorf("worktreeBranchPrefix must not start with -")
	}
	if strings.HasPrefix(trimmed, ".") {
		return "", fmt.Errorf("worktreeBranchPrefix must not start with .")
	}
	if strings.Contains(trimmed, "/") {
		return "", fmt.Errorf("worktreeBranchPrefix must not contain /")
	}
	if strings.Contains(trimmed, "..") {
		return "", fmt.Errorf("worktreeBranchPrefix must not contain ..")
	}
	for _, r := range trimmed {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_' || r == '.' {
			continue
		}
		return "", fmt.Errorf("worktreeBranchPrefix contains invalid character %q", r)
	}
	return trimmed, nil
}

func sanitizeWorktreeBranchPrefix(value string) string {
	prefix, err := validateWorktreeBranchPrefix(value)
	if err == nil {
		return prefix
	}
	log.Printf(
		"settings: invalid worktreeBranchPrefix %q, using default %q: %v",
		value,
		DefaultSettings.WorktreeBranchPrefix,
		err,
	)
	return DefaultSettings.WorktreeBranchPrefix
}

func sanitizeOption(field, value, fallback string, allowed map[string]struct{}) string {
	trimmed := strings.TrimSpace(value)
	if _, ok := allowed[trimmed]; ok {
		return trimmed
	}
	log.Printf("settings: invalid %s %q, using default %q", field, value, fallback)
	return fallback
}

func sanitizeBinaryPath(field, value, fallback string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed != "" {
		return trimmed
	}
	log.Printf("settings: empty %s, using default %q", field, fallback)
	return fallback
}

func normalizeBinaryPath(value, fallback string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return fallback
	}
	return trimmed
}

func normalizeRecentWorkspaces(paths []string) []string {
	if len(paths) == 0 {
		return nil
	}

	recent := make([]string, 0, min(len(paths), 10))
	seen := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		trimmed := strings.TrimSpace(path)
		if trimmed == "" {
			continue
		}
		if _, ok := seen[trimmed]; ok {
			continue
		}
		seen[trimmed] = struct{}{}
		recent = append(recent, trimmed)
		if len(recent) == 10 {
			break
		}
	}
	if len(recent) == 0 {
		return nil
	}
	return recent
}

func joinAllowedValues(values map[string]struct{}) string {
	options := make([]string, 0, len(values))
	// Ordered candidate list so error messages render deterministically.
	// Any value in `values` not in this list is skipped — new enums must
	// be appended here.
	candidates := []string{
		"system", "light", "dark",
		"locale", "12-hour", "24-hour",
		"claude", "codex",
		"low", "medium", "high", "xhigh", "max",
		"local", "worktree",
	}
	for _, candidate := range candidates {
		if _, ok := values[candidate]; ok {
			options = append(options, candidate)
		}
	}
	return strings.Join(options, ", ")
}

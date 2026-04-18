package settings

import (
	"fmt"
	"log"
	"strings"
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
	allowedProviders = map[string]struct{}{
		"claude": {},
		"codex":  {},
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
	// allowedModes mirrors provider.AllInteractionModes.
	allowedModes = map[string]struct{}{
		"chat":       {},
		"plan":       {},
		"design":     {},
		"discussion": {},
	}
	// allowedContextWindows mirrors the CHECK constraint on
	// threads.context_window (see store/migrate.go::v13SQL).
	allowedContextWindows = map[int]struct{}{
		200000:  {},
		1000000: {},
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

	current.DefaultProvider = strings.TrimSpace(current.DefaultProvider)
	if err := validateOption("defaultProvider", current.DefaultProvider, allowedProviders); err != nil {
		return Settings{}, err
	}

	var err error
	current.DefaultModelClaude, err = validateRequiredString("defaultModelClaude", current.DefaultModelClaude)
	if err != nil {
		return Settings{}, err
	}
	current.DefaultModelCodex, err = validateRequiredString("defaultModelCodex", current.DefaultModelCodex)
	if err != nil {
		return Settings{}, err
	}

	current.ClaudeBinaryPath = normalizeBinaryPath(current.ClaudeBinaryPath, DefaultSettings.ClaudeBinaryPath)
	current.CodexBinaryPath = normalizeBinaryPath(current.CodexBinaryPath, DefaultSettings.CodexBinaryPath)
	current.RecentWorkspaces = normalizeRecentWorkspaces(current.RecentWorkspaces)
	current.ObservabilityOtlpEndpoint = strings.TrimSpace(current.ObservabilityOtlpEndpoint)

	current.DefaultReasoningEffort = strings.TrimSpace(current.DefaultReasoningEffort)
	if err := validateOption(
		"defaultReasoningEffort",
		current.DefaultReasoningEffort,
		allowedReasoningEfforts,
	); err != nil {
		return Settings{}, err
	}

	current.DefaultMode = strings.TrimSpace(current.DefaultMode)
	if err := validateOption("defaultMode", current.DefaultMode, allowedModes); err != nil {
		return Settings{}, err
	}

	if _, ok := allowedContextWindows[current.DefaultContextWindow]; !ok {
		return Settings{}, fmt.Errorf("defaultContextWindow must be one of 200000, 1000000")
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
	current.DefaultProvider = sanitizeOption(
		"defaultProvider",
		current.DefaultProvider,
		DefaultSettings.DefaultProvider,
		allowedProviders,
	)
	current.DefaultModelClaude = sanitizeRequiredString(
		"defaultModelClaude",
		current.DefaultModelClaude,
		DefaultSettings.DefaultModelClaude,
	)
	current.DefaultModelCodex = sanitizeRequiredString(
		"defaultModelCodex",
		current.DefaultModelCodex,
		DefaultSettings.DefaultModelCodex,
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

	current.DefaultReasoningEffort = sanitizeOption(
		"defaultReasoningEffort",
		current.DefaultReasoningEffort,
		DefaultSettings.DefaultReasoningEffort,
		allowedReasoningEfforts,
	)
	current.DefaultMode = sanitizeOption(
		"defaultMode",
		current.DefaultMode,
		DefaultSettings.DefaultMode,
		allowedModes,
	)
	if _, ok := allowedContextWindows[current.DefaultContextWindow]; !ok {
		log.Printf(
			"settings: invalid defaultContextWindow %d, using default %d",
			current.DefaultContextWindow,
			DefaultSettings.DefaultContextWindow,
		)
		current.DefaultContextWindow = DefaultSettings.DefaultContextWindow
	}

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
	return current
}

func validateOption(field, value string, allowed map[string]struct{}) error {
	if _, ok := allowed[value]; !ok {
		return fmt.Errorf("%s must be one of %s", field, joinAllowedValues(allowed))
	}
	return nil
}

func validateRequiredString(field, value string) (string, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "", fmt.Errorf("%s cannot be empty", field)
	}
	return trimmed, nil
}

func sanitizeOption(field, value, fallback string, allowed map[string]struct{}) string {
	trimmed := strings.TrimSpace(value)
	if _, ok := allowed[trimmed]; ok {
		return trimmed
	}
	log.Printf("settings: invalid %s %q, using default %q", field, value, fallback)
	return fallback
}

func sanitizeRequiredString(field, value, fallback string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed != "" {
		return trimmed
	}
	log.Printf("settings: empty %s, using default %q", field, fallback)
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
		"chat", "plan", "design", "discussion",
	}
	for _, candidate := range candidates {
		if _, ok := values[candidate]; ok {
			options = append(options, candidate)
		}
	}
	return strings.Join(options, ", ")
}

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
	// Text-generation efforts are provider-specific. Codex app-server has no
	// "max"; Claude has no "none" or "minimal". Duplicated here to keep
	// internal/settings dependency-free of the provider package.
	allowedCodexTextGenerationEfforts = map[string]struct{}{
		"none":    {},
		"minimal": {},
		"low":     {},
		"medium":  {},
		"high":    {},
		"xhigh":   {},
	}
	allowedClaudeTextGenerationEfforts = map[string]struct{}{
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
	allowedPaneDensities = map[string]struct{}{
		"compact":     {},
		"comfortable": {},
		"spacious":    {},
	}
	// allowedFonts enumerates the typefaces selectable for --font-sans
	// and --font-mono. "geist" is the eager default, "hack-nerd" lazy-
	// loads, and "system" uses the OS fallback chain.
	allowedFonts = map[string]struct{}{
		"geist":     {},
		"hack-nerd": {},
		"system":    {},
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

	current.SansFont = strings.TrimSpace(current.SansFont)
	if err := validateOption("sansFont", current.SansFont, allowedFonts); err != nil {
		return Settings{}, err
	}

	current.MonoFont = strings.TrimSpace(current.MonoFont)
	if err := validateOption("monoFont", current.MonoFont, allowedFonts); err != nil {
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

	current.PaneDensity = strings.TrimSpace(current.PaneDensity)
	if err := validateOption("paneDensity", current.PaneDensity, allowedPaneDensities); err != nil {
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
	if err := validateTextGenerationReasoningEffort(
		current.TextGenerationProvider,
		current.TextGenerationReasoningEffort,
	); err != nil {
		return Settings{}, err
	}

	// Editor.Preference is open-vocabulary because detection mints
	// dynamic IDs (catalog editors plus "env:editor" / "env:visual"
	// fallbacks). We trim but don't enum-validate — an unknown value
	// just falls through to the catalog priority at resolve time, no
	// schema migration required when a new editor lands.
	current.Editor.Preference = strings.TrimSpace(current.Editor.Preference)

	// Auto-compact thresholds. Range 1..90 with the slider — values
	// outside that range are caller error.
	if err := validateAutoCompactPercent(
		"claudeAutoCompactStandardPercent",
		current.ClaudeAutoCompactStandardPercent,
	); err != nil {
		return Settings{}, err
	}
	if err := validateAutoCompactPercent(
		"claudeAutoCompactExtendedPercent",
		current.ClaudeAutoCompactExtendedPercent,
	); err != nil {
		return Settings{}, err
	}
	if err := validateAutoCompactPercent(
		"codexAutoCompactStandardPercent",
		current.CodexAutoCompactStandardPercent,
	); err != nil {
		return Settings{}, err
	}
	if err := validateAutoCompactPercent(
		"codexAutoCompactExtendedPercent",
		current.CodexAutoCompactExtendedPercent,
	); err != nil {
		return Settings{}, err
	}
	if err := validateRetentionDays(current.Retention.Days); err != nil {
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
	current.SansFont = sanitizeOption(
		"sansFont",
		current.SansFont,
		DefaultSettings.SansFont,
		allowedFonts,
	)
	current.MonoFont = sanitizeOption(
		"monoFont",
		current.MonoFont,
		DefaultSettings.MonoFont,
		allowedFonts,
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
	current.PaneDensity = sanitizeOption(
		"paneDensity",
		current.PaneDensity,
		DefaultSettings.PaneDensity,
		allowedPaneDensities,
	)

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
		allowedTextGenerationEfforts(current.TextGenerationProvider),
	)
	current.Editor.Preference = strings.TrimSpace(current.Editor.Preference)

	// Auto-compact thresholds: clamp on load so a hand-edited file with
	// an out-of-range value doesn't strand sessions on a degenerate
	// percent. The slider can't produce these values; this is a
	// belt-and-suspenders pass for the manual-edit path.
	current.ClaudeAutoCompactStandardPercent = sanitizeAutoCompactPercent(
		"claudeAutoCompactStandardPercent",
		current.ClaudeAutoCompactStandardPercent,
		DefaultSettings.ClaudeAutoCompactStandardPercent,
	)
	current.ClaudeAutoCompactExtendedPercent = sanitizeAutoCompactPercent(
		"claudeAutoCompactExtendedPercent",
		current.ClaudeAutoCompactExtendedPercent,
		DefaultSettings.ClaudeAutoCompactExtendedPercent,
	)
	current.CodexAutoCompactStandardPercent = sanitizeAutoCompactPercent(
		"codexAutoCompactStandardPercent",
		current.CodexAutoCompactStandardPercent,
		DefaultSettings.CodexAutoCompactStandardPercent,
	)
	current.CodexAutoCompactExtendedPercent = sanitizeAutoCompactPercent(
		"codexAutoCompactExtendedPercent",
		current.CodexAutoCompactExtendedPercent,
		DefaultSettings.CodexAutoCompactExtendedPercent,
	)
	current.Retention.Days = sanitizeRetentionDays(current.Retention.Days)
	return current
}

func validateAutoCompactPercent(field string, value int) error {
	if value < 1 || value > 90 {
		return fmt.Errorf("%s must be between 1 and 90", field)
	}
	return nil
}

func sanitizeAutoCompactPercent(field string, value, fallback int) int {
	if value >= 1 && value <= 90 {
		return value
	}
	log.Printf("settings: invalid %s %d, using default %d", field, value, fallback)
	return fallback
}

// MaxRetentionDays caps the retention window at ~100 years. The hard
// ceiling exists because the sweep computes `time.Duration(days) * 24
// * time.Hour`, which overflows int64 nanoseconds around 106_751 days
// and produces a positive Duration after negation — a cutoff in the
// future would delete every row. Capping well below that boundary
// keeps the arithmetic well-defined without constraining any plausible
// human-scale retention policy.
const MaxRetentionDays = 36500

// validateRetentionDays rejects negative and arithmetic-unsafe values.
// Zero is legal — it means "disable the sweep" — but a negative
// integer is a caller bug because there is no policy a negative window
// could represent, and a value above MaxRetentionDays risks Duration
// overflow downstream.
func validateRetentionDays(value int) error {
	if value < 0 {
		return fmt.Errorf("retention.days must be non-negative, got %d", value)
	}
	if value > MaxRetentionDays {
		return fmt.Errorf("retention.days must be at most %d, got %d", MaxRetentionDays, value)
	}
	return nil
}

// sanitizeRetentionDays clamps a hand-edited out-of-range value rather
// than rejecting the whole file at load. Negatives clamp to 0
// (disabled); over-cap values clamp to MaxRetentionDays. Matches the
// auto-compact sanitize pattern — load-time leniency, write-time
// strictness.
func sanitizeRetentionDays(value int) int {
	if value < 0 {
		log.Printf("settings: invalid retention.days %d, disabling sweep", value)
		return 0
	}
	if value > MaxRetentionDays {
		log.Printf("settings: retention.days %d exceeds %d, clamping", value, MaxRetentionDays)
		return MaxRetentionDays
	}
	return value
}

func validateOption(field, value string, allowed map[string]struct{}) error {
	if _, ok := allowed[value]; !ok {
		return fmt.Errorf("%s must be one of %s", field, joinAllowedValues(allowed))
	}
	return nil
}

func validateTextGenerationReasoningEffort(provider, effort string) error {
	if _, ok := allowedTextGenerationEfforts(provider)[effort]; ok {
		return nil
	}
	return fmt.Errorf(
		"textGenerationReasoningEffort must be one of %s for provider %q",
		joinAllowedValues(allowedTextGenerationEfforts(provider)),
		provider,
	)
}

func allowedTextGenerationEfforts(provider string) map[string]struct{} {
	switch provider {
	case "claude":
		return allowedClaudeTextGenerationEfforts
	default:
		return allowedCodexTextGenerationEfforts
	}
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
		"geist", "hack-nerd",
	}
	for _, candidate := range candidates {
		if _, ok := values[candidate]; ok {
			options = append(options, candidate)
		}
	}
	return strings.Join(options, ", ")
}

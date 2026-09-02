package settings

import (
	"fmt"
	"log"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

var (
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
	// Text-generation efforts are provider-specific. Claude has no "none",
	// "minimal", or "ultra". Duplicated here to keep internal/settings
	// dependency-free of the provider package.
	allowedCodexTextGenerationEfforts = map[string]struct{}{
		"none":    {},
		"minimal": {},
		"low":     {},
		"medium":  {},
		"high":    {},
		"xhigh":   {},
		"max":     {},
		"ultra":   {},
	}
	// allowedCommitMessageStyles enumerates the writing-style guidance
	// for generated commit messages. Values mirror
	// commitmsg.Style{Conventional,Custom,Repo}.
	allowedCommitMessageStyles = map[string]struct{}{
		"conventional": {},
		"custom":       {},
		"repo":         {},
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
	allowedActivityRunDefaults = map[string]struct{}{
		"expanded":  {},
		"collapsed": {},
	}
	allowedProjectSortModes = map[string]struct{}{
		"lastActivity": {},
		"createdAt":    {},
		"manual":       {},
	}
	allowedUsagePeriods = map[string]struct{}{
		"day":   {},
		"week":  {},
		"month": {},
		"all":   {},
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
	current.GitLabSelfHostedHosts, err = validateGitLabHosts(current.GitLabSelfHostedHosts)
	if err != nil {
		return Settings{}, err
	}
	current.WorktreeBranchPrefix, err = validateWorktreeBranchPrefix(current.WorktreeBranchPrefix)
	if err != nil {
		return Settings{}, err
	}
	current.BrowserChromiumPath, err = validateChromiumPath(current.BrowserChromiumPath)
	if err != nil {
		return Settings{}, err
	}
	// The generic patch path cannot CHANGE this key (update refuses it),
	// but it does re-validate the whole struct, so the rules stay stated
	// in one place rather than only on the SetNetwork path.
	current.Network, err = validateNetwork(current.Network)
	if err != nil {
		return Settings{}, err
	}

	current.PaneDensity = strings.TrimSpace(current.PaneDensity)
	if err := validateOption("paneDensity", current.PaneDensity, allowedPaneDensities); err != nil {
		return Settings{}, err
	}

	current.ActivityRunDefault = strings.TrimSpace(current.ActivityRunDefault)
	if err := validateOption(
		"activityRunDefault",
		current.ActivityRunDefault,
		allowedActivityRunDefaults,
	); err != nil {
		return Settings{}, err
	}
	if err := validateActivityRunWindowRows(current.ActivityRunWindowRows); err != nil {
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

	current.CommitMessageStyle = strings.TrimSpace(current.CommitMessageStyle)
	if err := validateOption(
		"commitMessageStyle",
		current.CommitMessageStyle,
		allowedCommitMessageStyles,
	); err != nil {
		return Settings{}, err
	}
	current.CommitMessageStyleCustom = strings.TrimSpace(current.CommitMessageStyleCustom)
	if err := validateCommitMessageStyleCustom(current.CommitMessageStyleCustom); err != nil {
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
	if err := validateFontSize("fontSize", current.FontSize); err != nil {
		return Settings{}, err
	}

	current.ProjectSortMode = strings.TrimSpace(current.ProjectSortMode)
	if err := validateOption("projectSortMode", current.ProjectSortMode, allowedProjectSortModes); err != nil {
		return Settings{}, err
	}
	current.UsagePeriod = strings.TrimSpace(current.UsagePeriod)
	if err := validateOption("usagePeriod", current.UsagePeriod, allowedUsagePeriods); err != nil {
		return Settings{}, err
	}
	if len(current.ClaudeHiddenModels) > MaxHiddenModels {
		return Settings{}, fmt.Errorf(
			"claudeHiddenModels has %d entries, max is %d",
			len(current.ClaudeHiddenModels), MaxHiddenModels,
		)
	}
	if len(current.CodexHiddenModels) > MaxHiddenModels {
		return Settings{}, fmt.Errorf(
			"codexHiddenModels has %d entries, max is %d",
			len(current.CodexHiddenModels), MaxHiddenModels,
		)
	}
	current.ClaudeHiddenModels = dedupeTrimmed(current.ClaudeHiddenModels, MaxHiddenModels)
	current.CodexHiddenModels = dedupeTrimmed(current.CodexHiddenModels, MaxHiddenModels)

	current.ClaudeCustomEnv, err = validateProviderEnvVars("claude", current.ClaudeCustomEnv)
	if err != nil {
		return Settings{}, err
	}
	current.CodexCustomEnv, err = validateProviderEnvVars("codex", current.CodexCustomEnv)
	if err != nil {
		return Settings{}, err
	}

	current.ClaudePromptOverrides, err = validatePromptOverrides("claudePromptOverrides", current.ClaudePromptOverrides)
	if err != nil {
		return Settings{}, err
	}
	current.CodexPromptOverrides, err = validatePromptOverrides("codexPromptOverrides", current.CodexPromptOverrides)
	if err != nil {
		return Settings{}, err
	}
	current.ClaudeDisabledTools, err = validateDisabledTools("claudeDisabledTools", current.ClaudeDisabledTools)
	if err != nil {
		return Settings{}, err
	}
	current.CodexDisabledTools, err = validateDisabledTools("codexDisabledTools", current.CodexDisabledTools)
	if err != nil {
		return Settings{}, err
	}

	current.ClaudeOutputStyle, err = validateClaudeOutputStyle("claudeOutputStyle", current.ClaudeOutputStyle)
	if err != nil {
		return Settings{}, err
	}
	current.ClaudeCrossSession, err = validateClaudeCrossSession("claudeCrossSession", current.ClaudeCrossSession)
	if err != nil {
		return Settings{}, err
	}
	current.ClaudeSubagentLimits, err = validateClaudeSubagentLimits("claudeSubagentLimits", current.ClaudeSubagentLimits)
	if err != nil {
		return Settings{}, err
	}
	current.ClaudeToolMemoryLimit, err = validateClaudeToolMemoryLimit("claudeToolMemoryLimit", current.ClaudeToolMemoryLimit)
	if err != nil {
		return Settings{}, err
	}
	current.ClaudeThinking, err = validateClaudeThinking("claudeThinking", current.ClaudeThinking)
	if err != nil {
		return Settings{}, err
	}

	current.SpinnerCustomVerbs, err = validateSpinnerCustomVerbs("spinnerCustomVerbs", current.SpinnerCustomVerbs)
	if err != nil {
		return Settings{}, err
	}
	current.SpinnerDisabledAnimations, err = validateSpinnerDisabledAnimations(
		"spinnerDisabledAnimations",
		current.SpinnerDisabledAnimations,
	)
	if err != nil {
		return Settings{}, err
	}
	current.SpinnerCompactionAnimation, err = validateSpinnerCompactionAnimation(
		"spinnerCompactionAnimation",
		current.SpinnerCompactionAnimation,
	)
	if err != nil {
		return Settings{}, err
	}

	return current, nil
}

func sanitizeLoadedSettings(current Settings) Settings {
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
	current.BrowserChromiumPath = sanitizeChromiumPath(current.BrowserChromiumPath)
	current.GitLabSelfHostedHosts = sanitizeGitLabHosts(current.GitLabSelfHostedHosts)
	current.Network = sanitizeNetwork(current.Network)
	current.ClaudeHiddenModels = dedupeTrimmed(current.ClaudeHiddenModels, MaxHiddenModels)
	current.CodexHiddenModels = dedupeTrimmed(current.CodexHiddenModels, MaxHiddenModels)
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
	current.ActivityRunDefault = sanitizeOption(
		"activityRunDefault",
		current.ActivityRunDefault,
		DefaultSettings.ActivityRunDefault,
		allowedActivityRunDefaults,
	)
	current.ActivityRunWindowRows = sanitizeActivityRunWindowRows(current.ActivityRunWindowRows)

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
	current.CommitMessageStyle = sanitizeOption(
		"commitMessageStyle",
		current.CommitMessageStyle,
		DefaultSettings.CommitMessageStyle,
		allowedCommitMessageStyles,
	)
	current.CommitMessageStyleCustom = sanitizeCommitMessageStyleCustom(current.CommitMessageStyleCustom)
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
	current.FontSize = sanitizeFontSize(
		"fontSize",
		current.FontSize,
		DefaultSettings.FontSize,
	)
	current.ProjectSortMode = sanitizeOption(
		"projectSortMode",
		current.ProjectSortMode,
		DefaultSettings.ProjectSortMode,
		allowedProjectSortModes,
	)
	current.UsagePeriod = sanitizeOption(
		"usagePeriod",
		current.UsagePeriod,
		DefaultSettings.UsagePeriod,
		allowedUsagePeriods,
	)
	current.ClaudeCustomEnv = sanitizeProviderEnvVars("claude", current.ClaudeCustomEnv)
	current.CodexCustomEnv = sanitizeProviderEnvVars("codex", current.CodexCustomEnv)
	current.ClaudePromptOverrides = sanitizePromptOverrides("claudePromptOverrides", current.ClaudePromptOverrides)
	current.CodexPromptOverrides = sanitizePromptOverrides("codexPromptOverrides", current.CodexPromptOverrides)
	current.ClaudeDisabledTools = sanitizeDisabledTools("claudeDisabledTools", current.ClaudeDisabledTools)
	current.CodexDisabledTools = sanitizeDisabledTools("codexDisabledTools", current.CodexDisabledTools)
	current.ClaudeOutputStyle = sanitizeClaudeOutputStyle("claudeOutputStyle", current.ClaudeOutputStyle)
	current.ClaudeCrossSession = sanitizeClaudeCrossSession("claudeCrossSession", current.ClaudeCrossSession)
	current.ClaudeSubagentLimits = sanitizeClaudeSubagentLimits("claudeSubagentLimits", current.ClaudeSubagentLimits)
	current.ClaudeToolMemoryLimit = sanitizeClaudeToolMemoryLimit("claudeToolMemoryLimit", current.ClaudeToolMemoryLimit)
	current.ClaudeThinking = sanitizeClaudeThinking("claudeThinking", current.ClaudeThinking)
	current.SpinnerCustomVerbs = sanitizeSpinnerCustomVerbs("spinnerCustomVerbs", current.SpinnerCustomVerbs)
	current.SpinnerDisabledAnimations = sanitizeSpinnerDisabledAnimations(
		"spinnerDisabledAnimations",
		current.SpinnerDisabledAnimations,
	)
	current.SpinnerCompactionAnimation = sanitizeSpinnerCompactionAnimation(
		"spinnerCompactionAnimation",
		current.SpinnerCompactionAnimation,
	)
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

const (
	MinFontSize = 10
	MaxFontSize = 20
)

// Bounds for ActivityRunWindowRows. The floor keeps the tail window large
// enough to overfill a run's height cap -- below it the clip would show
// blank space under the last mounted row. The ceiling is a DOM-cost guard:
// the whole point of the window is that a run's mounted rows stay O(K)
// regardless of how long the run is.
const (
	MinActivityRunWindowRows = 10
	MaxActivityRunWindowRows = 200
)

func validateActivityRunWindowRows(value int) error {
	if value < MinActivityRunWindowRows || value > MaxActivityRunWindowRows {
		return fmt.Errorf(
			"activityRunWindowRows must be between %d and %d",
			MinActivityRunWindowRows,
			MaxActivityRunWindowRows,
		)
	}
	return nil
}

// sanitizeActivityRunWindowRows clamps rather than falling back to the
// default: an out-of-range value still expresses a direction (the user
// wanted more or fewer rows), and the nearest legal value honors that.
// Matches sanitizeRetentionDays; contrast sanitizeFontSize, where an
// out-of-range point size carries no usable intent.
func sanitizeActivityRunWindowRows(value int) int {
	if value < MinActivityRunWindowRows {
		log.Printf(
			"settings: activityRunWindowRows %d below %d, clamping",
			value, MinActivityRunWindowRows,
		)
		return MinActivityRunWindowRows
	}
	if value > MaxActivityRunWindowRows {
		log.Printf(
			"settings: activityRunWindowRows %d exceeds %d, clamping",
			value, MaxActivityRunWindowRows,
		)
		return MaxActivityRunWindowRows
	}
	return value
}

func validateFontSize(field string, value int) error {
	if value < MinFontSize || value > MaxFontSize {
		return fmt.Errorf("%s must be between %d and %d", field, MinFontSize, MaxFontSize)
	}
	return nil
}

func sanitizeFontSize(field string, value, fallback int) int {
	if value >= MinFontSize && value <= MaxFontSize {
		return value
	}
	log.Printf("settings: invalid %s %d, using default %d", field, value, fallback)
	return fallback
}

func validateOption(field, value string, allowed map[string]struct{}) error {
	if _, ok := allowed[value]; !ok {
		return fmt.Errorf("%s must be one of %s", field, joinAllowedValues(allowed))
	}
	return nil
}

// ValidateTextGenerationReasoningEffort is the exported form of the check
// validateSettings runs, for callers that need to test one field without a
// whole Settings round trip. Mirrors ValidateProviderEnvVarName.
func ValidateTextGenerationReasoningEffort(provider, effort string) error {
	return validateTextGenerationReasoningEffort(provider, effort)
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

// maxCommitMessageStyleCustomLen bounds the free-text style
// instructions. The prompt layer caps what it embeds anyway
// (commitmsg.PromptCustomStyleLimit); this keeps the settings file from
// carrying an accidentally-pasted document.
const maxCommitMessageStyleCustomLen = 4_000

func validateCommitMessageStyleCustom(value string) error {
	if len(value) > maxCommitMessageStyleCustomLen {
		return fmt.Errorf(
			"commitMessageStyleCustom must be at most %d bytes (got %d)",
			maxCommitMessageStyleCustomLen,
			len(value),
		)
	}
	return nil
}

// sanitizeCommitMessageStyleCustom trims and hard-caps the stored
// instructions on load — a hand-edited file over the cap is truncated
// (on a rune boundary) rather than rejected wholesale.
func sanitizeCommitMessageStyleCustom(value string) string {
	trimmed := strings.TrimSpace(value)
	if len(trimmed) <= maxCommitMessageStyleCustomLen {
		return trimmed
	}
	log.Printf(
		"settings: commitMessageStyleCustom over %d bytes, truncating",
		maxCommitMessageStyleCustomLen,
	)
	return truncateRuneSafe(trimmed, maxCommitMessageStyleCustomLen)
}

func allowedTextGenerationEfforts(provider string) map[string]struct{} {
	switch provider {
	case "claude":
		return allowedClaudeTextGenerationEfforts
	default:
		return allowedCodexTextGenerationEfforts
	}
}

// AllowedTextGenerationEfforts returns the reasoning-effort slugs this package
// accepts for a text-generation provider, sorted. Order carries no meaning —
// error messages order the set through joinAllowedValues — the MEMBERSHIP is
// the contract.
//
// Exported for TestTextGenerationEffortsMatchTheProviderSets in the root
// package, the only place that can see both this copy and
// provider.ReasoningEffortsForProvider: internal/settings must not import
// internal/provider (cycle), so the tables above are duplicates, and this is
// the same arrangement ReservedProviderEnvNames uses for the env deny-list.
func AllowedTextGenerationEfforts(provider string) []string {
	allowed := allowedTextGenerationEfforts(provider)
	efforts := make([]string, 0, len(allowed))
	for effort := range allowed {
		efforts = append(efforts, effort)
	}
	sort.Strings(efforts)
	return efforts
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

// validateChromiumPath checks the headless engine's browser override.
//
// Empty is the normal value and means "find one on PATH" — the setting
// exists for the machine where discovery cannot: a browser outside PATH, or
// two installed and the wrong one first. A non-empty value must be ABSOLUTE,
// because a bare name would be resolved against the serve process's own PATH
// and an operator naming a browser deserves to get that browser rather than
// whichever one a service manager's environment happens to reach.
//
// Whether the file exists is deliberately NOT checked here: settings are
// validated at write and at every load, the browser is installed and
// upgraded independently of both, and a rule that fails a whole settings
// save because a package was mid-upgrade would be worse than the boot log
// that names the missing binary (internal/browser/headless_binary.go).
func validateChromiumPath(value string) (string, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "", nil
	}
	if !filepath.IsAbs(trimmed) {
		return "", fmt.Errorf("browserChromiumPath must be an absolute path to a Chromium executable, got %q", trimmed)
	}
	return trimmed, nil
}

// sanitizeChromiumPath is the load-path half: a hand-edited file with a
// relative path loses the override rather than failing the load, and says so.
func sanitizeChromiumPath(value string) string {
	cleaned, err := validateChromiumPath(value)
	if err != nil {
		log.Printf("settings: %v; ignoring it and searching PATH", err)
		return ""
	}
	return cleaned
}

func normalizeBinaryPath(value, fallback string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return fallback
	}
	return trimmed
}

// MaxGitLabSelfHostedHosts caps the allowlist length. A self-hosted-
// GitLab user usually has one or two corporate hosts; an upper bound of
// 50 leaves comfortable headroom while keeping the linear scan in
// classifyOriginURL cheap.
const MaxGitLabSelfHostedHosts = 50

// validateGitLabHosts strictly validates and normalises the gitlab-host
// allowlist on Update. Hosts are trimmed, lowercased, deduped, and
// rejected if they look like anything other than a bare hostname.
// Empty list / nil round-trip as nil (omitted from sparse JSON).
func validateGitLabHosts(hosts []string) ([]string, error) {
	if len(hosts) == 0 {
		return nil, nil
	}
	if len(hosts) > MaxGitLabSelfHostedHosts {
		return nil, fmt.Errorf(
			"gitlabSelfHostedHosts has %d entries, max is %d",
			len(hosts), MaxGitLabSelfHostedHosts,
		)
	}
	out := make([]string, 0, len(hosts))
	seen := make(map[string]struct{}, len(hosts))
	for _, raw := range hosts {
		host := strings.ToLower(strings.TrimSpace(raw))
		if host == "" {
			return nil, fmt.Errorf("gitlabSelfHostedHosts contains an empty entry")
		}
		if err := validateBareHostname(host); err != nil {
			return nil, fmt.Errorf("gitlabSelfHostedHosts: %w", err)
		}
		if host == "github.com" || host == "gitlab.com" {
			return nil, fmt.Errorf(
				"gitlabSelfHostedHosts must not contain %q (already recognised by literal match)",
				host,
			)
		}
		if _, dup := seen[host]; dup {
			continue
		}
		seen[host] = struct{}{}
		out = append(out, host)
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// sanitizeGitLabHosts is the lenient load-time counterpart to
// validateGitLabHosts. Invalid entries are dropped (with a log line) so
// a hand-edited settings.json with one bad host doesn't strand the
// whole allowlist. Caps the slice length defensively.
func sanitizeGitLabHosts(hosts []string) []string {
	if len(hosts) == 0 {
		return nil
	}
	out := make([]string, 0, len(hosts))
	seen := make(map[string]struct{}, len(hosts))
	for _, raw := range hosts {
		host := strings.ToLower(strings.TrimSpace(raw))
		if host == "" {
			continue
		}
		if err := validateBareHostname(host); err != nil {
			log.Printf("settings: dropping invalid gitlabSelfHostedHosts entry %q: %v", raw, err)
			continue
		}
		if host == "github.com" || host == "gitlab.com" {
			log.Printf("settings: dropping redundant gitlabSelfHostedHosts entry %q (already recognised)", host)
			continue
		}
		if _, dup := seen[host]; dup {
			continue
		}
		seen[host] = struct{}{}
		out = append(out, host)
		if len(out) >= MaxGitLabSelfHostedHosts {
			break
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// RFC 1035 §3.1 caps an FQDN at 255 octets in the wire encoding, which
// works out to 253 visible characters (one trailing length byte + a
// root-label null byte). Per-label cap is 63 octets. Enforcing both
// keeps allowlist entries within the same bounds the OS resolver and
// `git remote` use, so we reject malformed inputs early instead of
// memoising a host that could never match a real origin URL.
const (
	maxHostnameLength  = 253
	maxHostLabelLength = 63
)

// validateBareHostname rejects inputs that aren't a bare DNS hostname.
// We deliberately stay conservative: ASCII letters/digits/dot/hyphen
// only, no scheme, no path, no port, no spaces. This matches the
// canonicalisation extractRemoteHost performs on origin URLs, so an
// allowlist entry compares equal to the host extracted from any
// well-formed git remote URL.
func validateBareHostname(host string) error {
	if host == "" {
		return fmt.Errorf("hostname is empty")
	}
	if len(host) > maxHostnameLength {
		return fmt.Errorf("hostname is %d characters, max is %d", len(host), maxHostnameLength)
	}
	if strings.Contains(host, "://") {
		return fmt.Errorf("%q must not include a scheme", host)
	}
	if strings.ContainsAny(host, "/?#@:") {
		return fmt.Errorf("%q must be a bare hostname (no scheme, path, port, or userinfo)", host)
	}
	for _, r := range host {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '.' || r == '-':
		default:
			return fmt.Errorf("%q contains invalid character %q", host, r)
		}
	}
	if strings.HasPrefix(host, ".") || strings.HasSuffix(host, ".") {
		return fmt.Errorf("%q must not start or end with a dot", host)
	}
	if strings.HasPrefix(host, "-") || strings.HasSuffix(host, "-") {
		return fmt.Errorf("%q must not start or end with a hyphen", host)
	}
	if strings.Contains(host, "..") {
		return fmt.Errorf("%q must not contain consecutive dots", host)
	}
	if !strings.Contains(host, ".") {
		return fmt.Errorf("%q must contain at least one dot", host)
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) > maxHostLabelLength {
			return fmt.Errorf("hostname label %q is %d characters, max is %d", label, len(label), maxHostLabelLength)
		}
	}
	return nil
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

// MaxHiddenModels caps the per-provider hidden-model lists. Catalogs
// hold well under a hundred models; anything beyond this is a corrupt
// or hand-mangled file.
const MaxHiddenModels = 100

// dedupeTrimmed trims each entry, drops empties and duplicates, and
// caps the result at limit entries. Returns nil when nothing survives
// so sparse serialization omits the field.
func dedupeTrimmed(values []string, limit int) []string {
	if len(values) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			continue
		}
		if _, dup := seen[trimmed]; dup {
			continue
		}
		seen[trimmed] = struct{}{}
		out = append(out, trimmed)
		if len(out) >= limit {
			break
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// truncateRuneSafe cuts s to at most maxBytes, never through the middle of
// a rune AT THE CUT POINT. It is a length bound, not a sanitizer: bytes
// before the cut are passed through exactly as stored, including invalid
// ones. Validating the whole prefix instead would let a single stray byte
// anywhere in a hand-edited value collapse it to nothing.
func truncateRuneSafe(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	cut := s[:maxBytes]
	if utf8.RuneStart(s[maxBytes]) {
		// The next byte begins a rune, so the cut already fell on a
		// boundary — the common case, and the only one for ASCII.
		return cut
	}
	// s[maxBytes] continues a rune that started inside cut. Drop back past
	// this rune's continuation bytes AND its start byte; a rune is at most
	// utf8.UTFMax bytes, so that is a bounded walk, not a search.
	for i := 0; i < utf8.UTFMax-1 && len(cut) > 0; i++ {
		last := cut[len(cut)-1]
		cut = cut[:len(cut)-1]
		if utf8.RuneStart(last) {
			break
		}
	}
	return cut
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
		"none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra",
		"local", "worktree",
		"compact", "comfortable", "spacious",
		"geist", "hack-nerd",
		"lastActivity", "createdAt", "manual",
	}
	for _, candidate := range candidates {
		if _, ok := values[candidate]; ok {
			options = append(options, candidate)
		}
	}
	return strings.Join(options, ", ")
}

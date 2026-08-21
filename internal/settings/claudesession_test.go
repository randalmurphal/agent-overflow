package settings

import "testing"

func TestValidateClaudeOutputStyleAllowlist(t *testing.T) {
	for _, ok := range []string{"", "Concise", "Proactive", "Explanatory", "Learning"} {
		got, err := validateClaudeOutputStyle("claudeOutputStyle", ok)
		if err != nil {
			t.Fatalf("validateClaudeOutputStyle(%q) = %v, want nil", ok, err)
		}
		if got != ok {
			t.Fatalf("validateClaudeOutputStyle(%q) = %q", ok, got)
		}
	}
	// Case matters: the CLI matches its built-in table by exact name, so a
	// lowercase value would save cleanly and then be silently ignored.
	for _, bad := range []string{"concise", "default", "MyCustomStyle"} {
		if _, err := validateClaudeOutputStyle("claudeOutputStyle", bad); err == nil {
			t.Fatalf("validateClaudeOutputStyle(%q) = nil error, want refusal", bad)
		}
	}
}

func TestValidateClaudeSubagentLimits(t *testing.T) {
	// Zero is "let the CLI decide", not "no subagents" — the CLI's own
	// schema is int({min:1}), so zero can never be sent.
	if _, err := validateClaudeSubagentLimits("claudeSubagentLimits", ClaudeSubagentLimits{}); err != nil {
		t.Fatalf("zero limits = %v, want accepted", err)
	}
	if _, err := validateClaudeSubagentLimits("claudeSubagentLimits", ClaudeSubagentLimits{MaxSpawnDepth: 3, MaxConcurrent: 8}); err != nil {
		t.Fatalf("in-range limits = %v", err)
	}
	for _, bad := range []ClaudeSubagentLimits{
		{MaxSpawnDepth: -1},
		{MaxConcurrent: -1},
		{MaxSpawnDepth: MaxClaudeSubagentLimit + 1},
		{MaxConcurrent: MaxClaudeSubagentLimit + 1},
	} {
		if _, err := validateClaudeSubagentLimits("claudeSubagentLimits", bad); err == nil {
			t.Fatalf("validateClaudeSubagentLimits(%+v) = nil error, want refusal", bad)
		}
	}
}

// The grammar is the CLI's own regex plus its falsy-word set. A value it
// would silently ignore must be refused at the save instead.
func TestValidateClaudeToolMemoryLimitGrammar(t *testing.T) {
	for _, ok := range []string{"", "4G", "512m", "2GiB", "1.5g", "1024", "1024b", "none", "off", "0", "false", "no"} {
		if _, err := validateClaudeToolMemoryLimit("claudeToolMemoryLimit", ok); err != nil {
			t.Fatalf("validateClaudeToolMemoryLimit(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"4 gigs", "G", "-1G", "4G!", "lots"} {
		if _, err := validateClaudeToolMemoryLimit("claudeToolMemoryLimit", bad); err == nil {
			t.Fatalf("validateClaudeToolMemoryLimit(%q) = nil error, want refusal", bad)
		}
	}
}

// The lenient load path must degrade to the CLI default rather than make
// a settings file written by a newer build unloadable.
func TestSanitizeClaudeSessionAxesDropsUnknownValues(t *testing.T) {
	if got := sanitizeClaudeOutputStyle("claudeOutputStyle", "SomeFutureStyle"); got != "" {
		t.Fatalf("sanitizeClaudeOutputStyle = %q, want empty", got)
	}
	if got := sanitizeClaudeToolMemoryLimit("claudeToolMemoryLimit", "loads"); got != "" {
		t.Fatalf("sanitizeClaudeToolMemoryLimit = %q, want empty", got)
	}
	got := sanitizeClaudeSubagentLimits("claudeSubagentLimits", ClaudeSubagentLimits{MaxSpawnDepth: 9999, MaxConcurrent: 4})
	if got.MaxSpawnDepth != 0 || got.MaxConcurrent != 4 {
		t.Fatalf("sanitizeClaudeSubagentLimits = %+v, want the bad half dropped and the good half kept", got)
	}
}

// Claude-only by construction: claude-tui's PTY launch passes no
// `--settings`, so routing it onto these values would promise an effect
// that never lands.
func TestClaudeSessionAxesForProviderIsClaudeOnly(t *testing.T) {
	s := Settings{
		ClaudeOutputStyle:     "Concise",
		ClaudeSubagentLimits:  ClaudeSubagentLimits{MaxSpawnDepth: 2, MaxConcurrent: 6},
		ClaudeToolMemoryLimit: "4G",
	}
	axes := s.ClaudeSessionAxesForProvider("claude")
	if axes.OutputStyle != "Concise" ||
		axes.SubagentLimits.MaxSpawnDepth != 2 || axes.SubagentLimits.MaxConcurrent != 6 ||
		axes.ToolMemoryLimit != "4G" {
		t.Fatalf("claude axes = %+v", axes)
	}
	for _, other := range []string{"claude-tui", "codex", ""} {
		if got := s.ClaudeSessionAxesForProvider(other); got != (ClaudeSessionAxes{}) {
			t.Fatalf("ClaudeSessionAxesForProvider(%q) = %+v, want zero", other, got)
		}
	}
}

// Zero values must stay out of the persisted file so an older settings
// file (and a downgrade) keeps reading as "CLI default".
func TestClaudeSessionAxesAreSparse(t *testing.T) {
	sparse, err := buildSparseMap(Settings{})
	if err != nil {
		t.Fatalf("buildSparseMap: %v", err)
	}
	for _, key := range []string{"claudeOutputStyle", "claudeToolMemoryLimit"} {
		if _, present := sparse[key]; present {
			t.Fatalf("%s present in a sparse write of zero settings", key)
		}
	}
	// The struct axis cannot use omitempty, so it is the one that has to
	// be proven equal-to-default rather than absent-by-tag.
	if _, present := sparse["claudeSubagentLimits"]; present {
		t.Fatal("claudeSubagentLimits present in a sparse write of zero settings")
	}
}

// The budget bound is Agent Overflow's, not the CLI's — the control-request
// handler takes any integer — so it is the only thing standing between a
// typo and a session whose every request the API rejects.
func TestValidateClaudeThinking(t *testing.T) {
	for _, ok := range []ClaudeThinking{
		{},
		{Mode: ClaudeThinkingModeOff},
		{Mode: ClaudeThinkingModeOff, Display: ClaudeThinkingDisplayOmitted},
		{Display: ClaudeThinkingDisplaySummarized},
		{Mode: ClaudeThinkingModeBudget, BudgetTokens: MinClaudeThinkingBudgetTokens},
		{Mode: ClaudeThinkingModeBudget, BudgetTokens: 2048, Display: ClaudeThinkingDisplayOmitted},
		{Mode: ClaudeThinkingModeBudget, BudgetTokens: MaxClaudeThinkingBudgetTokens},
	} {
		if _, err := validateClaudeThinking("claudeThinking", ok); err != nil {
			t.Fatalf("validateClaudeThinking(%+v) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []ClaudeThinking{
		{Mode: "disabled"},
		{Mode: "adaptive"},
		{Mode: "Off"},
		{Display: "hidden"},
		{Display: "Summarized"},
		// A budget mode is meaningless without a usable budget, and
		// coercing it would report a save the user never made.
		{Mode: ClaudeThinkingModeBudget},
		{Mode: ClaudeThinkingModeBudget, BudgetTokens: MinClaudeThinkingBudgetTokens - 1},
		{Mode: ClaudeThinkingModeBudget, BudgetTokens: MaxClaudeThinkingBudgetTokens + 1},
		{Mode: ClaudeThinkingModeBudget, BudgetTokens: -1},
	} {
		if _, err := validateClaudeThinking("claudeThinking", bad); err == nil {
			t.Fatalf("validateClaudeThinking(%+v) = nil error, want refusal", bad)
		}
	}
}

// A budget stored beside a non-budget mode would read as configuration and
// behave as nothing — `--max-thinking-tokens` is never rendered for those
// modes. Dropping it keeps the persisted shape honest.
func TestValidateClaudeThinkingDropsBudgetOutsideBudgetMode(t *testing.T) {
	for _, mode := range []string{ClaudeThinkingModeDefault, ClaudeThinkingModeOff} {
		got, err := validateClaudeThinking("claudeThinking", ClaudeThinking{Mode: mode, BudgetTokens: 4096})
		if err != nil {
			t.Fatalf("validateClaudeThinking(mode %q) = %v", mode, err)
		}
		if got.BudgetTokens != 0 {
			t.Fatalf("validateClaudeThinking(mode %q).BudgetTokens = %d, want 0", mode, got.BudgetTokens)
		}
	}
}

// The lenient half: a value a newer build wrote (or a hand edit produced)
// degrades to "Claude Code decides" instead of making the file unloadable.
func TestSanitizeClaudeThinkingDropsUnusableValues(t *testing.T) {
	for _, bad := range []ClaudeThinking{
		{Mode: "adaptive"},
		{Mode: ClaudeThinkingModeBudget, BudgetTokens: 10},
		{Display: "hidden"},
	} {
		if got := sanitizeClaudeThinking("claudeThinking", bad); got != (ClaudeThinking{}) {
			t.Fatalf("sanitizeClaudeThinking(%+v) = %+v, want zero", bad, got)
		}
	}
	keep := ClaudeThinking{Mode: ClaudeThinkingModeBudget, BudgetTokens: 2048, Display: ClaudeThinkingDisplayOmitted}
	if got := sanitizeClaudeThinking("claudeThinking", keep); got != keep {
		t.Fatalf("sanitizeClaudeThinking(%+v) = %+v, want it kept", keep, got)
	}
}

// Headless claude only. claude-tui accepts the spawn flags but has no
// control-request channel, so honoring it there would advertise a live
// setting that is silently spawn-only on one of the two Claude transports.
func TestClaudeThinkingForProviderIsHeadlessClaudeOnly(t *testing.T) {
	s := Settings{ClaudeThinking: ClaudeThinking{Mode: ClaudeThinkingModeBudget, BudgetTokens: 2048}}
	if got := s.ClaudeThinkingForProvider("claude"); got != s.ClaudeThinking {
		t.Fatalf("ClaudeThinkingForProvider(claude) = %+v", got)
	}
	for _, other := range []string{"claude-tui", "codex", ""} {
		if got := s.ClaudeThinkingForProvider(other); got != (ClaudeThinking{}) {
			t.Fatalf("ClaudeThinkingForProvider(%q) = %+v, want zero", other, got)
		}
	}
}

// Sparse like every other axis here: a zero value must not reach the file,
// or an older build (and a downgrade) would read it as a real choice.
func TestClaudeThinkingIsSparse(t *testing.T) {
	sparse, err := buildSparseMap(Settings{})
	if err != nil {
		t.Fatalf("buildSparseMap: %v", err)
	}
	if _, present := sparse["claudeThinking"]; present {
		t.Fatal("claudeThinking present in a sparse write of zero settings")
	}
	sparse, err = buildSparseMap(Settings{ClaudeThinking: ClaudeThinking{Mode: ClaudeThinkingModeOff}})
	if err != nil {
		t.Fatalf("buildSparseMap: %v", err)
	}
	if _, present := sparse["claudeThinking"]; !present {
		t.Fatal("claudeThinking absent from a sparse write that set it")
	}
}

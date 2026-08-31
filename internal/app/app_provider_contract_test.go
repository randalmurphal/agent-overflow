package app

import (
	"context"
	"slices"
	"strings"
	"testing"

	"agent-overflow/internal/claudecatalog"
	"agent-overflow/internal/codexapp"
	"agent-overflow/internal/codexskills"
	"agent-overflow/internal/codexusage"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/settings"
)

func TestProviderLeafBindingsPreserveShutdownSentinel(t *testing.T) {
	app := NewApp()
	app.shuttingDown.Store(true)
	assertSame := func(name string, err error) {
		t.Helper()
		if err != ErrShuttingDown {
			t.Fatalf("%s error = %v, want ErrShuttingDown", name, err)
		}
	}
	_, err := app.GetThreadContextUsage("thread")
	assertSame("GetThreadContextUsage", err)
	assertSame("StopClaudeTask", app.StopClaudeTask("thread", "task"))
	assertSame("BackgroundClaudeTask", app.BackgroundClaudeTask("thread", "tool"))
	assertSame("CleanCodexBackgroundTerminals", app.CleanCodexBackgroundTerminals("thread"))
	terminated, err := app.TerminateCodexBackgroundTerminal("thread", "process")
	assertSame("TerminateCodexBackgroundTerminal", err)
	if terminated {
		t.Fatal("TerminateCodexBackgroundTerminal returned true while shutting down")
	}
}

func TestProviderLeafBindingDTOProjectionsAreFieldComplete(t *testing.T) {
	contextUsage := projectThreadContextUsage(&claude.ContextUsage{
		TotalTokens: 10, MaxTokens: 100, Percentage: 10, Model: "claude-test",
		Categories: []claude.ContextUsageCategory{{Name: "Deferred", Tokens: 5, Deferred: true}},
	})
	if !contextUsage.Available || contextUsage.TotalTokens != 10 || contextUsage.MaxTokens != 100 ||
		contextUsage.Percentage != 10 || contextUsage.Model != "claude-test" ||
		len(contextUsage.Categories) != 1 || !contextUsage.Categories[0].Deferred {
		t.Fatalf("context usage projection = %+v", contextUsage)
	}

	app := NewApp()
	value := int64(42)
	usageCache := codexusage.New()
	usageKey := "codex-test\x00account-1"
	if _, err := usageCache.Get(context.Background(), usageKey, func(context.Context) (codex.AccountUsage, error) {
		return codex.AccountUsage{
			LifetimeTokens: &value,
			DailyBuckets:   []codex.AccountUsageDailyBucket{{StartDate: "2026-08-01", Tokens: 42}},
		}, nil
	}); err != nil {
		t.Fatalf("prime usage cache: %v", err)
	}
	app.codexAppOnce.Do(func() {
		app.codexApp = codexapp.New(codexapp.Deps{
			Binary: func() string { return "codex-test" },
			ActiveAccount: func() codexapp.AccountSelection {
				return codexapp.AccountSelection{ID: "account-1", Email: "user@example.com"}
			},
			UsageCache: usageCache,
		})
	})
	usage, err := app.GetCodexAccountUsage()
	if err != nil || usage == nil || usage.LifetimeTokens == nil || *usage.LifetimeTokens != 42 ||
		usage.AccountEmail != "user@example.com" || len(usage.DailyBuckets) != 1 ||
		usage.DailyBuckets[0].StartDate != "2026-08-01" {
		t.Fatalf("account usage projection = %+v, err=%v", usage, err)
	}
}

func TestProviderLeafBindingsAllocateWireSlices(t *testing.T) {
	claudecatalog.Reset()
	app := NewApp()
	commands := app.GetClaudeSlashCommands()
	if commands.Probed || commands.Commands == nil {
		t.Fatalf("commands = %+v, want unknown with allocated slice", commands)
	}
	capture := claudecatalog.CommandCapture{}
	capture.Capture([]provider.SlashCommand{{Name: "usage"}}, nil)
	capture.Store(app.claudeProbeModelKey())
	commands = app.GetClaudeSlashCommands()
	if !commands.Probed || len(commands.Commands) != 1 {
		t.Fatalf("commands after report = %+v", commands)
	}

	cwd := t.TempDir()
	cache := codexskills.New()
	if _, err := cache.Get(context.Background(), codexskills.Key("codex-test", cwd), func(context.Context) (codexskills.CwdSkills, error) {
		return codexskills.CwdSkills{Cwd: cwd}, nil
	}); err != nil {
		t.Fatalf("prime skills cache: %v", err)
	}
	app = NewApp()
	app.codexAppOnce.Do(func() {
		app.codexApp = codexapp.New(codexapp.Deps{
			Binary: func() string { return "codex-test" }, SkillsCache: cache,
		})
	})
	skills, err := app.GetCodexSkills(context.Background(), cwd, false)
	if err != nil || skills.Skills == nil || skills.Errors == nil {
		t.Fatalf("skills = %+v, err=%v", skills, err)
	}
}

// internal/settings cannot import internal/provider (cycle), so its
// per-provider reasoning-effort tables are copies of
// provider.ReasoningEffortsForProvider. This is the cross-check that keeps them
// honest in both directions.
//
// The failure it prevents is asymmetric and both halves are silent. A tier the
// provider package offers but settings rejects makes the settings UI refuse a
// value the composer's own picker hands out — Codex's max and ultra are exactly
// that pair, added to the provider enum and to the store CHECK (migration v19)
// before this table was widened. A tier settings accepts but no provider does
// would validate, persist, and then be coerced away at spawn, so the user's
// configured effort silently is not the one that runs.
func TestTextGenerationEffortsMatchTheProviderSets(t *testing.T) {
	// Text generation is gated to these two; claude-tui is never routed here.
	for _, providerName := range []string{string(provider.Claude), string(provider.Codex)} {
		t.Run(providerName, func(t *testing.T) {
			canonical := provider.ReasoningEffortsForProvider(providerName)
			allowed := settings.AllowedTextGenerationEfforts(providerName)

			for _, effort := range canonical {
				if !slices.Contains(allowed, string(effort)) {
					t.Errorf("%s offers effort %q but settings rejects it for text generation", providerName, effort)
				}
			}
			for _, effort := range allowed {
				if !slices.Contains(canonical, provider.ReasoningEffort(effort)) {
					t.Errorf("settings accepts %q for %s but the provider does not offer it; the value would be coerced away at spawn", effort, providerName)
				}
			}

			// Every accepted slug survives a real validation call, not merely
			// a map lookup, and every one is named in the message a rejected
			// value produces — the list the user is shown must not be shorter
			// than the list that works.
			for _, effort := range canonical {
				if err := settings.ValidateTextGenerationReasoningEffort(providerName, string(effort)); err != nil {
					t.Errorf("settings rejected %s/%s: %v", providerName, effort, err)
				}
			}
			err := settings.ValidateTextGenerationReasoningEffort(providerName, "ultranope")
			if err == nil {
				t.Fatalf("settings accepted an unknown effort for %s", providerName)
			}
			for _, effort := range canonical {
				if !strings.Contains(err.Error(), string(effort)) {
					t.Errorf("the rejection message for %s omits the legal tier %q: %s", providerName, effort, err)
				}
			}
		})
	}
}

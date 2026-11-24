package app

import (
	"context"
	"testing"

	"agent-overflow/internal/claudecatalog"
	"agent-overflow/internal/codexapp"
	"agent-overflow/internal/codexskills"
	"agent-overflow/internal/codexusage"
	"agent-overflow/internal/procutil/procutiltest"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/provider/codex"
)

func TestProviderLeafBindingsPreserveShutdownSentinel(t *testing.T) {
	t.Parallel()
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
	stopped, err := app.StopClaudeTask("thread", "task")
	assertSame("StopClaudeTask", err)
	if stopped {
		t.Fatal("StopClaudeTask returned true while shutting down")
	}
	assertSame("BackgroundClaudeTask", app.BackgroundClaudeTask("thread", "tool"))
	assertSame("cleanCodexBackgroundTerminals", app.cleanCodexBackgroundTerminals("thread"))
	terminated, err := app.TerminateCodexBackgroundTerminal("thread", "process")
	assertSame("TerminateCodexBackgroundTerminal", err)
	if terminated {
		t.Fatal("TerminateCodexBackgroundTerminal returned true while shutting down")
	}
}

func TestProviderLeafBindingDTOProjectionsAreFieldComplete(t *testing.T) {
	t.Parallel()
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
			Reaper: procutiltest.Reaper(),
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
	t.Cleanup(claudecatalog.Reset)
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
	if _, err := cache.Get(context.Background(), codexskills.Key("codex-test", cwd), func() (codexskills.CwdSkills, error) {
		return codexskills.CwdSkills{Cwd: cwd}, nil
	}); err != nil {
		t.Fatalf("prime skills cache: %v", err)
	}
	app = NewApp()
	app.codexAppOnce.Do(func() {
		app.codexApp = codexapp.New(codexapp.Deps{
			Reaper: procutiltest.Reaper(),
			Binary: func() string { return "codex-test" }, SkillsCache: cache,
		})
	})
	skills, err := app.GetCodexSkills(context.Background(), cwd, false)
	if err != nil || skills.Skills == nil || skills.Errors == nil {
		t.Fatalf("skills = %+v, err=%v", skills, err)
	}
}

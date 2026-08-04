package main

import (
	"errors"
	"testing"

	"agent-overflow/internal/provider"
)

func newAppForClaudeCommandTest(t *testing.T) *App {
	t.Helper()
	// One probe fills the account, model, and command caches, so they reset
	// together — see resetClaudeCommandCacheForTest.
	resetClaudeProbeCacheForTest()
	return newTestAppWithStore(t)
}

// TestGetClaudeSlashCommandsReportsUnknownBeforeAnyProbe is the whole reason
// the wire shape is a struct: an unprobed identity and a binary that genuinely
// has no commands both read back as an empty list, and a composer must render
// the first as "I don't know yet" rather than emptying its menu.
func TestGetClaudeSlashCommandsReportsUnknownBeforeAnyProbe(t *testing.T) {
	app := newAppForClaudeCommandTest(t)

	got := app.GetClaudeSlashCommands()
	if got.Probed {
		t.Fatal("Probed must be false until a probe has actually reported")
	}
	if got.Commands == nil {
		t.Fatal("Commands must be an allocated slice so the wire carries [] rather than null")
	}
	if len(got.Commands) != 0 {
		t.Fatalf("Commands = %+v, want empty", got.Commands)
	}
}

// TestGetClaudeSlashCommandsReturnsTheProbedList covers the transition the
// menu depends on: unknown → reported. The rich fields have to survive,
// because names alone are what the live session frames already carry.
func TestGetClaudeSlashCommandsReturnsTheProbedList(t *testing.T) {
	app := newAppForClaudeCommandTest(t)

	app.storeClaudeWireCommands(app.claudeProbeModelKey(), []provider.SlashCommand{
		{Name: "usage", Description: "Show plan usage", ArgumentHint: ""},
		{Name: "context", Description: "Show context breakdown"},
	}, nil)

	got := app.GetClaudeSlashCommands()
	if !got.Probed {
		t.Fatal("Probed must be true once a probe has reported")
	}
	if len(got.Commands) != 2 {
		t.Fatalf("Commands = %+v, want 2 entries", got.Commands)
	}
	if got.Commands[0].Name != "usage" || got.Commands[0].Description != "Show plan usage" {
		t.Fatalf("first command = %+v, want the probed name and description", got.Commands[0])
	}
}

// TestGetClaudeSlashCommandsDistinguishesAnEmptyAnswerFromNoAnswer pins the
// other side of the same transition: a probe that reports no commands is an
// ANSWER, and it must not read back as "unknown" — otherwise a menu would keep
// waiting for a report that already arrived.
func TestGetClaudeSlashCommandsDistinguishesAnEmptyAnswerFromNoAnswer(t *testing.T) {
	app := newAppForClaudeCommandTest(t)

	app.storeClaudeWireCommands(app.claudeProbeModelKey(), nil, nil)

	got := app.GetClaudeSlashCommands()
	if !got.Probed {
		t.Fatal("a probe reporting no commands is an answer; Probed must be true")
	}
	if len(got.Commands) != 0 {
		t.Fatalf("Commands = %+v, want empty", got.Commands)
	}
}

// TestGetClaudeSlashCommandsLeavesTheListAloneOnAWireError guards the rule
// claudecommands.Store owns: an unreadable array is no information, so the
// previous answer stands rather than being replaced by an empty one.
func TestGetClaudeSlashCommandsLeavesTheListAloneOnAWireError(t *testing.T) {
	app := newAppForClaudeCommandTest(t)
	key := app.claudeProbeModelKey()

	app.storeClaudeWireCommands(key, []provider.SlashCommand{{Name: "usage"}}, nil)
	app.storeClaudeWireCommands(key, nil, errors.New("commands array unreadable"))

	got := app.GetClaudeSlashCommands()
	if !got.Probed || len(got.Commands) != 1 || got.Commands[0].Name != "usage" {
		t.Fatalf("after a wire error the previous answer must stand; got %+v", got)
	}
}

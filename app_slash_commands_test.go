package main

import (
	"encoding/json"
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

// TestGetThreadSlashCommands_EmptyWhenUnset covers the clean empty case: a
// thread that has never seen an EventInit carrying slash commands returns an
// empty slice (never nil) so the frontend popover doesn't have to null-check.
func TestGetThreadSlashCommands_EmptyWhenUnset(t *testing.T) {
	app := newTestAppWithStore(t)

	cmds, err := app.GetThreadSlashCommands("thread-no-init")
	if err != nil {
		t.Fatalf("GetThreadSlashCommands() error = %v", err)
	}
	if cmds == nil {
		t.Fatal("expected non-nil empty slice, got nil")
	}
	if len(cmds) != 0 {
		t.Fatalf("expected empty, got %v", cmds)
	}
}

// TestGetThreadSlashCommands_PopulatedByInitEvent proves the EventInit -> cache
// round trip: a Claude system.init carrying slash_commands populates the
// cache, and GetThreadSlashCommands returns a stable copy.
func TestGetThreadSlashCommands_PopulatedByInitEvent(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-slash-init")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	meta, err := json.Marshal(provider.SessionInfo{
		SessionID:     "s-1",
		Model:         "claude-opus-4-6",
		SlashCommands: []string{"init", "review", "test-runner"},
	})
	if err != nil {
		t.Fatalf("marshal session info: %v", err)
	}

	handler := app.sessionEventHandler(thread.ID, "token-init", "")
	handler(provider.ProviderEvent{
		Kind:      provider.EventInit,
		ThreadID:  thread.ID,
		Meta:      meta,
		Timestamp: time.Now(),
	})

	cmds, err := app.GetThreadSlashCommands(thread.ID)
	if err != nil {
		t.Fatalf("GetThreadSlashCommands() error = %v", err)
	}
	want := []string{"init", "review", "test-runner"}
	if len(cmds) != len(want) {
		t.Fatalf("GetThreadSlashCommands() len = %d, want %d (%v)", len(cmds), len(want), cmds)
	}
	for i, v := range want {
		if cmds[i] != v {
			t.Errorf("cmds[%d] = %q, want %q", i, cmds[i], v)
		}
	}

	// Mutating the returned slice must not affect future reads: the binding
	// returns a defensive copy, not the cached slice.
	cmds[0] = "mutated"
	again, err := app.GetThreadSlashCommands(thread.ID)
	if err != nil {
		t.Fatalf("GetThreadSlashCommands() second error = %v", err)
	}
	if again[0] != "init" {
		t.Errorf("cache was mutated: got %q, want %q", again[0], "init")
	}
}

// TestCacheSlashCommandsFromInit_EmptyListRemovesCache verifies that a fresh
// init without slash commands overwrites the previous list with emptiness
// rather than preserving stale state — this matters when a user removes
// ~/.claude/commands/*.md between sessions.
func TestCacheSlashCommandsFromInit_EmptyListRemovesCache(t *testing.T) {
	app := newTestAppWithStore(t)

	// Seed a non-empty cache.
	meta1, _ := json.Marshal(provider.SessionInfo{
		SessionID:     "s-1",
		SlashCommands: []string{"foo", "bar"},
	})
	app.cacheSlashCommandsFromInit("thread-1", meta1)

	before, _ := app.GetThreadSlashCommands("thread-1")
	if len(before) != 2 {
		t.Fatalf("setup: expected 2 cached commands, got %v", before)
	}

	// Simulate a subsequent init with an empty slash_commands field.
	meta2, _ := json.Marshal(provider.SessionInfo{SessionID: "s-2"})
	app.cacheSlashCommandsFromInit("thread-1", meta2)

	after, _ := app.GetThreadSlashCommands("thread-1")
	if len(after) != 0 {
		t.Fatalf("expected cache cleared, got %v", after)
	}
}

// TestCacheSlashCommandsFromInit_IgnoresMalformedMeta proves the cache write is
// robust against unparseable meta: a bad payload must not panic or partially
// mutate existing cache state.
func TestCacheSlashCommandsFromInit_IgnoresMalformedMeta(t *testing.T) {
	app := newTestAppWithStore(t)

	seed, _ := json.Marshal(provider.SessionInfo{
		SlashCommands: []string{"seeded"},
	})
	app.cacheSlashCommandsFromInit("thread-bad", seed)

	// Pass a deliberately broken JSON payload. Cache must be unchanged.
	app.cacheSlashCommandsFromInit("thread-bad", []byte(`{"slashCommands":not-json}`))

	cmds, err := app.GetThreadSlashCommands("thread-bad")
	if err != nil {
		t.Fatalf("GetThreadSlashCommands() error = %v", err)
	}
	if len(cmds) != 1 || cmds[0] != "seeded" {
		t.Fatalf("expected cache preserved, got %v", cmds)
	}
}

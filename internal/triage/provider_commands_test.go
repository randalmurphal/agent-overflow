package triage

import (
	"encoding/json"
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

func commandEmissions(emissions *emissionLog) []ProviderCommandsEvent {
	var out []ProviderCommandsEvent
	for _, e := range emissions.snapshot() {
		if e.eventName != "provider:commands" {
			continue
		}
		if payload, ok := e.data.(ProviderCommandsEvent); ok {
			out = append(out, payload)
		}
	}
	return out
}

func initEventWithCommands(threadID string, names []string) provider.ProviderEvent {
	meta, err := json.Marshal(provider.SessionInfo{
		SessionID:     "s1",
		Model:         "claude-opus-5",
		SlashCommands: names,
	})
	if err != nil {
		panic(err)
	}
	return provider.ProviderEvent{
		Kind:      provider.EventInit,
		ThreadID:  threadID,
		Meta:      meta,
		Timestamp: time.Now(),
	}
}

func commandsChangedEvent(threadID string, commands []provider.SlashCommand) provider.ProviderEvent {
	meta, err := json.Marshal(provider.CommandsChangedMeta{Commands: commands})
	if err != nil {
		panic(err)
	}
	return provider.ProviderEvent{
		Kind:      provider.EventCommandsChanged,
		ThreadID:  threadID,
		Meta:      meta,
		Timestamp: time.Now(),
	}
}

func TestProviderCommands_EmittedFromSystemInit(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	if err := router.Handle(initEventWithCommands("t1", []string{"usage", "mcp__playwright__browser_snapshot"})); err != nil {
		t.Fatalf("init: %v", err)
	}
	got := commandEmissions(emissions)
	if len(got) != 1 {
		t.Fatalf("command emissions = %d, want 1", len(got))
	}
	if got[0].ThreadID != "t1" || got[0].Provider != "claude" || !got[0].Replace {
		t.Fatalf("emission = %+v", got[0])
	}
	if len(got[0].Commands) != 2 || got[0].Commands[0].Name != "usage" {
		t.Fatalf("commands = %+v", got[0].Commands)
	}
	// init reports names only — triage must not invent descriptions.
	if got[0].Commands[0].Description != "" || got[0].Commands[0].ArgumentHint != "" {
		t.Fatalf("init-sourced command carried enrichment triage never saw: %+v", got[0].Commands[0])
	}
}

// A CLI too old to report commands must produce no frame at all: an empty
// frame would render as "this session has no commands", and absence is silence.
func TestProviderCommands_SilentInitEmitsNothing(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	if err := router.Handle(initEventWithCommands("t1", nil)); err != nil {
		t.Fatalf("init: %v", err)
	}
	if got := commandEmissions(emissions); len(got) != 0 {
		t.Fatalf("command emissions = %d, want 0", len(got))
	}
}

func TestProviderCommands_CommandsChangedIsAReplacement(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	err := router.Handle(commandsChangedEvent("t1", []provider.SlashCommand{
		{Name: "usage", Description: "Show plan usage limits"},
		{Name: "ship-it", Description: "Release checklist (user)", ArgumentHint: "[version]"},
	}))
	if err != nil {
		t.Fatalf("commands_changed: %v", err)
	}
	got := commandEmissions(emissions)
	if len(got) != 1 {
		t.Fatalf("command emissions = %d, want 1", len(got))
	}
	if !got[0].Replace {
		t.Fatal("commands_changed must be flagged as a replacement")
	}
	if got[0].Commands[1].ArgumentHint != "[version]" {
		t.Fatalf("rich fields lost: %+v", got[0].Commands[1])
	}
}

// An empty commands_changed IS a replacement — the parser only produces the
// event when the envelope carried a `commands` key — so it must reach the
// frontend and clear a live menu.
func TestProviderCommands_EmptyCommandsChangedStillEmits(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	if err := router.Handle(commandsChangedEvent("t1", nil)); err != nil {
		t.Fatalf("commands_changed: %v", err)
	}
	got := commandEmissions(emissions)
	if len(got) != 1 {
		t.Fatalf("command emissions = %d, want 1", len(got))
	}
	if got[0].Commands == nil {
		t.Fatal("empty replacement must carry [] so the wire is not null")
	}
	if len(got[0].Commands) != 0 {
		t.Fatalf("commands = %+v, want empty", got[0].Commands)
	}
}

// Ordering coverage, not just state coverage: a replacement followed by a
// session restart's init must leave the newest frame in charge either way
// round. Triage keeps no list of its own, so each frame is emitted verbatim
// and the LAST one wins — the property a menu depends on.
func TestProviderCommands_ReplaceThenInitOrdering(t *testing.T) {
	rich := []provider.SlashCommand{{Name: "ship-it", Description: "Release checklist (user)"}}

	t.Run("replace_then_init", func(t *testing.T) {
		router, st, emissions := newTestRouter(t)
		createTestThread(t, st, "t1")

		if err := router.Handle(commandsChangedEvent("t1", rich)); err != nil {
			t.Fatalf("commands_changed: %v", err)
		}
		if err := router.Handle(initEventWithCommands("t1", []string{"usage"})); err != nil {
			t.Fatalf("init: %v", err)
		}
		got := commandEmissions(emissions)
		if len(got) != 2 {
			t.Fatalf("command emissions = %d, want 2", len(got))
		}
		if len(got[1].Commands) != 1 || got[1].Commands[0].Name != "usage" {
			t.Fatalf("last frame = %+v, want the init list", got[1].Commands)
		}
	})

	t.Run("init_then_replace", func(t *testing.T) {
		router, st, emissions := newTestRouter(t)
		createTestThread(t, st, "t1")

		if err := router.Handle(initEventWithCommands("t1", []string{"usage"})); err != nil {
			t.Fatalf("init: %v", err)
		}
		if err := router.Handle(commandsChangedEvent("t1", rich)); err != nil {
			t.Fatalf("commands_changed: %v", err)
		}
		got := commandEmissions(emissions)
		if len(got) != 2 {
			t.Fatalf("command emissions = %d, want 2", len(got))
		}
		if len(got[1].Commands) != 1 || got[1].Commands[0].Name != "ship-it" {
			t.Fatalf("last frame = %+v, want the replacement list", got[1].Commands)
		}
	})
}

// The event must be live-only: nothing about it may reach SQLite.
func TestProviderCommands_PersistsNothing(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	if err := router.Handle(commandsChangedEvent("t1", []provider.SlashCommand{{Name: "usage"}})); err != nil {
		t.Fatalf("commands_changed: %v", err)
	}
	items, err := st.ListItems("t1")
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("items = %+v, want none", items)
	}
}

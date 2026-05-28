package main

import (
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/store"
)

func newDraftTestApp(t *testing.T) *App {
	t.Helper()

	app := newTestAppWithStore(t)

	thread := store.Thread{
		ID:            "thr-draft",
		ProjectID:     defaultTestProjectID,
		Title:         "Draft Thread",
		Provider:      "claude",
		WorkspacePath: "/tmp",
		Model:         "claude",
		Mode:          "chat",
		CreatedAt:     time.Now().UnixMilli(),
		UpdatedAt:     time.Now().UnixMilli(),
	}
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	return app
}

func TestSaveAndGetDraftRoundTrip(t *testing.T) {
	app := newDraftTestApp(t)

	chips := []TerminalChip{{
		ID:        "chip-1",
		Label:     "shell",
		Preview:   "$ ls",
		Content:   "$ ls\nREADME.md",
		CreatedAt: 1,
	}}

	if err := app.SaveDraft("thr-draft", "hello @file", []string{"att-1", "att-2"}, chips, nil); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}

	got, err := app.GetDraft("thr-draft")
	if err != nil {
		t.Fatalf("GetDraft: %v", err)
	}
	if got.Content != "hello @file" {
		t.Fatalf("Content: got %q", got.Content)
	}
	if len(got.AttachmentIDs) != 2 || got.AttachmentIDs[0] != "att-1" {
		t.Fatalf("AttachmentIDs: %+v", got.AttachmentIDs)
	}
	if len(got.TerminalChips) != 1 || got.TerminalChips[0].ID != "chip-1" {
		t.Fatalf("TerminalChips: %+v", got.TerminalChips)
	}
}

func TestSaveAndGetDraftRoundTripsSourceProposedPlan(t *testing.T) {
	app := newDraftTestApp(t)

	src := &SourceProposedPlan{
		ThreadID:  "src-thread",
		ItemID:    "plan-item-1",
		PayloadID: "payload-99",
		Title:     "Implement feature",
	}
	if err := app.SaveDraft("thr-draft", "PLEASE IMPLEMENT THIS PLAN:\n# Plan", nil, nil, src); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}

	got, err := app.GetDraft("thr-draft")
	if err != nil {
		t.Fatalf("GetDraft: %v", err)
	}
	if got.SourceProposedPlan == nil {
		t.Fatal("SourceProposedPlan: got nil, want round-tripped ref")
	}
	if got.SourceProposedPlan.ThreadID != src.ThreadID || got.SourceProposedPlan.ItemID != src.ItemID || got.SourceProposedPlan.PayloadID != src.PayloadID || got.SourceProposedPlan.Title != src.Title {
		t.Fatalf("SourceProposedPlan: got %+v, want %+v", got.SourceProposedPlan, src)
	}

	// Resaving with nil clears the column — the partial index in v31
	// stays selective only if empty refs round-trip as SQL NULL.
	if err := app.SaveDraft("thr-draft", "still here", nil, nil, nil); err != nil {
		t.Fatalf("SaveDraft (clear): %v", err)
	}
	got2, err := app.GetDraft("thr-draft")
	if err != nil {
		t.Fatalf("GetDraft after clear: %v", err)
	}
	if got2.SourceProposedPlan != nil {
		t.Fatalf("SourceProposedPlan after nil save: got %+v, want nil", got2.SourceProposedPlan)
	}
}

func TestGetDraftMissingReturnsEmpty(t *testing.T) {
	app := newDraftTestApp(t)

	got, err := app.GetDraft("thr-draft")
	if err != nil {
		t.Fatalf("GetDraft: %v", err)
	}
	if got.Content != "" {
		t.Fatalf("expected empty content for missing draft, got %q", got.Content)
	}
	if got.AttachmentIDs == nil || len(got.AttachmentIDs) != 0 {
		t.Fatalf("expected non-nil empty AttachmentIDs, got %+v", got.AttachmentIDs)
	}
	if got.TerminalChips == nil || len(got.TerminalChips) != 0 {
		t.Fatalf("expected non-nil empty TerminalChips, got %+v", got.TerminalChips)
	}
}

func TestSaveDraftOverwrites(t *testing.T) {
	app := newDraftTestApp(t)

	if err := app.SaveDraft("thr-draft", "v1", nil, nil, nil); err != nil {
		t.Fatalf("SaveDraft v1: %v", err)
	}
	if err := app.SaveDraft("thr-draft", "v2", []string{"att-9"}, nil, nil); err != nil {
		t.Fatalf("SaveDraft v2: %v", err)
	}

	got, err := app.GetDraft("thr-draft")
	if err != nil {
		t.Fatalf("GetDraft: %v", err)
	}
	if got.Content != "v2" {
		t.Fatalf("Content: got %q want %q", got.Content, "v2")
	}
	if len(got.AttachmentIDs) != 1 || got.AttachmentIDs[0] != "att-9" {
		t.Fatalf("AttachmentIDs: %+v", got.AttachmentIDs)
	}
}

func TestClearDraftRemovesRow(t *testing.T) {
	app := newDraftTestApp(t)

	if err := app.SaveDraft("thr-draft", "to clear", nil, nil, nil); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	if err := app.ClearDraft("thr-draft"); err != nil {
		t.Fatalf("ClearDraft: %v", err)
	}
	if err := app.ClearDraft("thr-draft"); err != nil {
		t.Fatalf("ClearDraft idempotent: %v", err)
	}

	got, err := app.GetDraft("thr-draft")
	if err != nil {
		t.Fatalf("GetDraft: %v", err)
	}
	if got.Content != "" {
		t.Fatalf("expected empty content after clear, got %q", got.Content)
	}
}

func TestSaveDraftRequiresInitialisedStore(t *testing.T) {
	app := &App{}
	err := app.SaveDraft("thr", "", nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "not initialized") {
		t.Fatalf("expected init error, got %v", err)
	}
}

func TestGetDraftHandlesBadStoredJSON(t *testing.T) {
	app := newDraftTestApp(t)

	// Directly poison the row with invalid JSON; the binding should surface
	// a clear error rather than panicking.
	err := app.store.UpsertThreadDraft(store.ThreadDraft{
		ThreadID:      "thr-draft",
		Content:       "bad",
		Attachments:   "not-json",
		TerminalChips: "[]",
		UpdatedAt:     1,
	})
	if err != nil {
		t.Fatalf("UpsertThreadDraft: %v", err)
	}

	if _, err := app.GetDraft("thr-draft"); err == nil {
		t.Fatal("expected error decoding bad JSON")
	}
}

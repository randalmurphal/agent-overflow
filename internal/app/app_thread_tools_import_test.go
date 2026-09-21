//go:build !providersmoke

package app

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/threadmode"
	"agent-overflow/internal/threadtools"
)

// TestThreadSearchFindsAnImportedCodexSessionThroughTheTool runs the whole
// path an agent takes to reach history this app never wrote: a real Codex
// rollout on disk, the real import, and then `thread_search` as a session
// calls it, decoded from the tool's own result rather than read off the
// store.
//
// The phrase lives only in the rollout, so a hit can come from nowhere but
// the imported-history tables behind `timeline_items`.
func TestThreadSearchFindsAnImportedCodexSessionThroughTheTool(t *testing.T) {
	const phrase = "quokka telemetry"
	app := newTestAppWithStore(t)
	app.configDir = t.TempDir()
	home := newImportHome(t)
	home.attach(app)
	home.writeCodexIndex(t, importFixtureCodexThread)
	home.writeCodexRollout(t, importFixtureCodexThread, append(
		[]string{codexFixtureLine(t, 0, "session_meta", map[string]any{
			"id": importFixtureCodexThread, "cwd": home.workspace, "originator": "codex_cli",
			"cli_version": "0.146.0", "git": map[string]any{"branch": "main"},
		})},
		codexFixtureTurn(t, "turn-1",
			"why does the "+phrase+" exporter drop spans?",
			"The "+phrase+" exporter drops spans when the queue is full.", 100)...,
	)...)

	if _, err := app.ListImportableSessions(ImportScanRequest{}); err != nil {
		t.Fatalf("ListImportableSessions: %v", err)
	}
	frames := runImport(t, app, "codex:"+importFixtureCodexThread)
	if len(frames) < 2 || len(frames[0].ThreadIDs) != 1 {
		t.Fatalf("import frames = %+v, want one imported thread", frames)
	}
	imported := frames[0].ThreadIDs[0]

	// The index is written inside the import transaction; the build is run
	// so the flag the tool reports is the settled one a boot leaves behind.
	if err := app.store.BuildSearchIndex(t.Context()); err != nil {
		t.Fatalf("BuildSearchIndex: %v", err)
	}

	caller, err := createTestThread(t, app, string(provider.Claude), t.TempDir(), "claude-opus-4-7", threadmode.ModeChat)
	if err != nil {
		t.Fatalf("create the calling thread: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	answer, err := app.threadToolsServer().Call(ctx,
		threadtools.Caller{ThreadID: caller.ID, Title: caller.Title, ComputerID: "local-computer", ComputerName: "This Mac"},
		"thread_search", json.RawMessage(`{"query":"\"`+phrase+`\"","limit":10}`))
	if err != nil {
		t.Fatalf("thread_search: %v", err)
	}
	var result struct {
		Indexing bool `json:"indexing"`
		Rows     []struct {
			ThreadID string `json:"thread_id"`
			Provider string `json:"provider"`
			ItemID   string `json:"item_id"`
			Snippet  string `json:"snippet"`
		} `json:"rows"`
	}
	encoded, err := json.Marshal(answer)
	if err != nil {
		t.Fatalf("encode thread_search result: %v", err)
	}
	if err := json.Unmarshal(encoded, &result); err != nil {
		t.Fatalf("decode thread_search result %s: %v", encoded, err)
	}
	if result.Indexing {
		t.Error("a settled index still reported a build in progress")
	}
	var hit bool
	for _, row := range result.Rows {
		if row.ThreadID != imported {
			continue
		}
		hit = true
		if row.Provider != string(provider.Codex) {
			t.Errorf("the imported hit reads as provider %q", row.Provider)
		}
		if row.ItemID == "" {
			t.Error("the hit names no item to open with thread_show")
		}
		if !strings.Contains(strings.ToLower(row.Snippet), phrase) {
			t.Errorf("snippet = %q, want the phrase from the rollout", row.Snippet)
		}
	}
	if !hit {
		t.Fatalf("thread_search(%q) = %s, want the imported thread %s", phrase, encoded, imported)
	}
}

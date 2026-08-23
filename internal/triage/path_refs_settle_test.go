package triage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"agent-overflow/internal/pathlinks"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// TestSettleStreamingTextEnrichesPathRefs is the integration-shaped
// test that the path-link enrichment hook actually fires from
// text settlement and writes a validated allowlist onto the
// persisted item's meta. The text contains both a real file path
// (relative to the seeded workspace) and a bogus token (`bogus.nope`)
// that would have passed the legacy client-side regex — only the real
// path should survive the validation pass.
func TestSettleStreamingTextEnrichesPathRefs(t *testing.T) {
	// Seed a temp workspace with one real file.
	wsRoot := t.TempDir()
	srcDir := filepath.Join(wsRoot, "src")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "foo.ts"), nil, 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	router, st, _ := newTestRouter(t)

	// Override createTestThread's hard-coded /tmp workspace by writing
	// the thread row directly with the seeded path.
	ensureTriageProject(t, st)
	now := time.Now().UnixMilli()
	if err := st.CreateThread(store.Thread{
		ID:            "t1",
		ProjectID:     triageTestProjectID,
		Title:         "path-refs",
		Provider:      "claude",
		WorkspacePath: wsRoot,
		CreatedAt:     now,
		UpdatedAt:     now,
	}); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	// Stream a text delta containing one real path and one shape-only
	// token. First delta creates the row with status=streaming.
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTextDelta,
		ThreadID:  "t1",
		Content:   "see src/foo.ts and bogus.nope/file.bad — done",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("text delta: %v", err)
	}

	// Close the block: text settlement and path-ref enrichment run,
	// persistItem writes the enriched meta. The settle is async on the
	// content-block-stop hot path, so wait before reading the row.
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventContentBlockStop,
		ThreadID:  "t1",
		Meta:      json.RawMessage(`{"blockType":"text"}`),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("content block stop: %v", err)
	}
	router.WaitForPendingSettles()

	row := firstItemByKind(t, st, "t1", itemKindAssistantText)
	if row.Meta == "" {
		t.Fatalf("expected non-empty meta after enrichment, got empty")
	}
	var meta struct {
		PathRefs []pathlinks.PathRef `json:"pathRefs"`
	}
	if err := json.Unmarshal([]byte(row.Meta), &meta); err != nil {
		t.Fatalf("unmarshal meta %q: %v", row.Meta, err)
	}
	if len(meta.PathRefs) != 1 {
		t.Fatalf("expected exactly one validated path ref, got %#v (raw meta=%q)", meta.PathRefs, row.Meta)
	}
	if meta.PathRefs[0].Path != "src/foo.ts" {
		t.Fatalf("expected src/foo.ts, got %#v", meta.PathRefs[0])
	}
}

// TestSettleStreamingTextSkipsEnrichmentWithoutWorkspace covers the
// degraded path: a thread row without a workspace can't validate
// relative paths. Settlement still has to succeed and persist the
// item; the meta just stays empty of pathRefs.
func TestSettleStreamingTextSkipsEnrichmentWithoutWorkspace(t *testing.T) {
	router, st, _ := newTestRouter(t)
	ensureTriageProject(t, st)
	now := time.Now().UnixMilli()
	if err := st.CreateThread(store.Thread{
		ID:            "t-no-ws",
		ProjectID:     triageTestProjectID,
		Title:         "no-workspace",
		Provider:      "claude",
		WorkspacePath: "",
		CreatedAt:     now,
		UpdatedAt:     now,
	}); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTextDelta,
		ThreadID:  "t-no-ws",
		Content:   "see src/foo.ts please",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("text delta: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventContentBlockStop,
		ThreadID:  "t-no-ws",
		Meta:      json.RawMessage(`{"blockType":"text"}`),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("content block stop: %v", err)
	}
	row := firstItemByKind(t, st, "t-no-ws", itemKindAssistantText)
	if row.Meta != "" && row.Meta != "{}" {
		// Tolerate either truly empty or `{}`; reject anything carrying pathRefs.
		var got map[string]json.RawMessage
		if err := json.Unmarshal([]byte(row.Meta), &got); err == nil {
			if _, has := got["pathRefs"]; has {
				t.Fatalf("expected no pathRefs when workspacePath empty, got meta=%q", row.Meta)
			}
		}
	}
}

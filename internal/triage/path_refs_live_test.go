package triage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/pathlinks"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// TestStreamingTextEmitsPathRefsMidStream proves the LIVE pathRefs
// validator fires from the byte-threshold flush in
// bufferStreamPersistence — not just from the settle path at
// content_block_stop. This is the regression guard for the
// "linkify AS it streams" bug.
//
// Drives three text deltas:
//
//  1. small first delta carries the path token; creates the row,
//     emits an upsert, does NOT pass through the buffer
//     (firstBlock=true never calls bufferTextPersistence)
//  2. second delta of non-path text large enough to trip
//     streamPersistByteThreshold; bufferTextPersistence flushes
//     synchronously, enrichStreamingPathRefsAndEmit runs against the
//     combined summary, sees src/foo.ts in the prior text, and emits
//     a meta event BEFORE we touch the block-stop boundary
//  3. another non-path delta over the threshold; flushes again but
//     the per-row dedupe cache short-circuits the meta emit because
//     the merged pathRefs JSON is byte-identical
//
// content_block_stop is intentionally NOT sent. The assertion is
// that the meta event already exists in the emissions buffer at
// this point — proving that linkification fires while the stream
// is in flight rather than only at settle.
func TestStreamingTextEmitsPathRefsMidStream(t *testing.T) {
	wsRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(wsRoot, "src"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wsRoot, "src", "foo.ts"), nil, 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	router, st, emissions := newTestRouter(t)
	ensureTriageProject(t, st)
	now := time.Now().UnixMilli()
	if err := st.CreateThread(store.Thread{
		ID:            "t1",
		ProjectID:     triageTestProjectID,
		Title:         "live-pathrefs",
		Provider:      "claude",
		WorkspacePath: wsRoot,
		CreatedAt:     now,
		UpdatedAt:     now,
	}); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	// Delta 1 — small, includes the path token. Creates the row
	// (firstBlock=true) and emits the upsert. The buffer is NOT
	// touched on the firstBlock branch, so no meta event yet.
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTextDelta,
		ThreadID:  "t1",
		Content:   "see src/foo.ts and ",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("delta 1: %v", err)
	}
	if got := filterItemEventMetas(*emissions); len(got) != 0 {
		t.Fatalf("expected 0 meta events after delta 1 (firstBlock bypasses buffer), got %d: %+v", len(got), got)
	}

	// Delta 2 — large enough to trip streamPersistByteThreshold.
	// bufferTextPersistence returns flushNow=true and synchronously
	// flushes the buffer; flushStreamPersistence calls
	// enrichStreamingPathRefsAndEmit which sees src/foo.ts in the
	// combined summary and emits action:"meta".
	padding := strings.Repeat("x", streamPersistByteThreshold+1)
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTextDelta,
		ThreadID:  "t1",
		Content:   padding,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("delta 2: %v", err)
	}

	metas := filterItemEventMetas(*emissions)
	if len(metas) != 1 {
		t.Fatalf("expected exactly 1 mid-stream meta event after delta 2 byte-threshold flush, got %d: %+v", len(metas), metas)
	}
	got := metas[0]
	if got.ThreadID != "t1" {
		t.Fatalf("meta threadId: got %q, want t1", got.ThreadID)
	}
	if got.Kind != itemKindAssistantText {
		t.Fatalf("meta kind: got %q, want %s", got.Kind, itemKindAssistantText)
	}
	if got.ItemID == "" {
		t.Fatalf("meta itemId empty; expected the streaming row id")
	}
	var metaPayload struct {
		PathRefs []pathlinks.PathRef `json:"pathRefs"`
	}
	if err := json.Unmarshal([]byte(got.Meta), &metaPayload); err != nil {
		t.Fatalf("unmarshal mid-stream meta %q: %v", got.Meta, err)
	}
	if len(metaPayload.PathRefs) != 1 || metaPayload.PathRefs[0].Path != "src/foo.ts" {
		t.Fatalf("expected single validated ref src/foo.ts, got %#v", metaPayload.PathRefs)
	}

	// The persisted row's meta must agree with the emitted shape so
	// a frontend that joins via the WebSocket gap (refreshFromBackend)
	// sees the same pathRefs the live emit pushed. Bonus: confirms the
	// flush ran synchronously inline (bufferStreamPersistence's
	// flushNow=true path) — if the flush had been deferred to the
	// 250ms timer, the persisted summary would still be the first
	// delta's content and this assertion would fail loudly rather
	// than racing.
	row, found, err := st.GetThreadItem("t1", got.ItemID)
	if err != nil || !found {
		t.Fatalf("expected persisted row %s after mid-stream flush; found=%v err=%v", got.ItemID, found, err)
	}
	if row.Meta != got.Meta {
		t.Fatalf("persisted meta vs emitted meta mismatch:\n  persisted=%q\n  emitted=%q", row.Meta, got.Meta)
	}
	if !strings.Contains(row.Summary, padding) {
		t.Fatalf("persisted summary did not absorb delta 2 — flush did not run inline (timer-only regression?)")
	}
	if row.Status != statusStreaming {
		t.Fatalf("mid-stream row status: got %q, want %q (settle did not run)", row.Status, statusStreaming)
	}

	// Delta 3 — same shape as delta 2; no new paths in the summary.
	// Flushes synchronously again, but enrichStreamingPathRefsAndEmit's
	// per-row dedupe cache (streamingPathRefsLast) makes
	// `previous == merged` and short-circuits before UpdateItemMeta
	// and the emit. The meta-event count must stay at 1.
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTextDelta,
		ThreadID:  "t1",
		Content:   padding,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("delta 3: %v", err)
	}
	if got := filterItemEventMetas(*emissions); len(got) != 1 {
		t.Fatalf("expected per-row dedupe to hold meta count at 1 across no-new-path flush; got %d: %+v", len(got), got)
	}
}

// TestStreamingTextDedupeSurvivesSettle pins the load-bearing defer
// ordering in doSettleStreamingText. The defer-clear-after-finishSettle
// stack means clearStreamingPathRefs runs AFTER the in-body
// flushStreamingItem, so the final-flush emit can see the prior merged
// hash and short-circuit when settle has nothing new to validate.
// A regression that cleared the cache BEFORE the final flush would
// emit a redundant action:"meta" + UpdateItemMeta for every settled
// text row carrying paths — silent on the wire but observable through
// the emission count.
//
// The test must NOT rely on delta2's inline threshold flush leaving an
// empty buffer at settle time — that path makes settle's flush a no-op
// (takeStreamPersistenceLocked returns nil) and `enrichStreamingPathRefsAndEmit`
// is never called, so a defer-order regression would slip through. We
// queue a third small (sub-threshold) delta whose bytes only commit on
// settle's flushStreamingItem call. That forces the settle-time enrich
// call where the dedupe cache state matters.
func TestStreamingTextDedupeSurvivesSettle(t *testing.T) {
	wsRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(wsRoot, "src"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wsRoot, "src", "foo.ts"), nil, 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	router, st, emissions := newTestRouter(t)
	ensureTriageProject(t, st)
	now := time.Now().UnixMilli()
	if err := st.CreateThread(store.Thread{
		ID:            "t1",
		ProjectID:     triageTestProjectID,
		Title:         "live-pathrefs-settle",
		Provider:      "claude",
		WorkspacePath: wsRoot,
		CreatedAt:     now,
		UpdatedAt:     now,
	}); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	// First two deltas: small with path + large to trip threshold.
	// After this, exactly one meta event has fired and the cache
	// holds the merged JSON.
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTextDelta,
		ThreadID:  "t1",
		Content:   "see src/foo.ts and ",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("delta 1: %v", err)
	}
	padding := strings.Repeat("x", streamPersistByteThreshold+1)
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTextDelta,
		ThreadID:  "t1",
		Content:   padding,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("delta 2: %v", err)
	}
	if got := filterItemEventMetas(*emissions); len(got) != 1 {
		t.Fatalf("expected 1 mid-stream meta after delta 2, got %d: %+v", len(got), got)
	}

	// Delta 3 — small, no new paths, BELOW threshold. The buffer
	// accumulates these bytes and schedules a 250ms timer flush but
	// does NOT flush inline. Settle is forced before the timer fires,
	// so settle's flushStreamingItem is the only path that drains the
	// buffer and triggers enrichStreamingPathRefsAndEmit. THAT'S the
	// emit whose dedupe behavior the defer-order invariant gates.
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTextDelta,
		ThreadID:  "t1",
		Content:   " tail",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("delta 3: %v", err)
	}

	// Settle. doSettleStreamingText runs flushStreamingItem against the
	// buffered delta-3 bytes; that flush calls enrichStreamingPathRefsAndEmit
	// against the row whose summary now includes delta3. No NEW path
	// validates (just appended " tail"), so the merged pathRefs JSON is
	// byte-identical to the cached one — previous == merged short-circuits
	// the emit. Then defer-clear fires (LIFO after finishSettle).
	// Regression mode: if clearStreamingPathRefs ran BEFORE flushStreamingItem
	// in doSettleStreamingText, previous would be "" and the dedupe would
	// fail open — count would jump from 1 to 2.
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventContentBlockStop,
		ThreadID:  "t1",
		Meta:      json.RawMessage(`{"blockType":"text"}`),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("content block stop: %v", err)
	}
	router.WaitForPendingSettles()

	if got := filterItemEventMetas(*emissions); len(got) != 1 {
		t.Fatalf("expected dedupe to hold meta count at 1 across settle's drain of delta 3; got %d: %+v", len(got), got)
	}

	// Post-settle row sanity: completed, summary absorbed delta 3,
	// pathRefs preserved.
	row := firstItemByKind(t, st, "t1", itemKindAssistantText)
	if row.Status != statusCompleted {
		t.Fatalf("post-settle status: got %q, want %q", row.Status, statusCompleted)
	}
	if !strings.HasSuffix(row.Summary, " tail") {
		t.Fatalf("post-settle summary did not absorb delta 3 (settle did not flush buffer); ends with %q", row.Summary[max(0, len(row.Summary)-32):])
	}
	var meta struct {
		PathRefs []pathlinks.PathRef `json:"pathRefs"`
	}
	if err := json.Unmarshal([]byte(row.Meta), &meta); err != nil {
		t.Fatalf("unmarshal post-settle meta %q: %v", row.Meta, err)
	}
	if len(meta.PathRefs) != 1 || meta.PathRefs[0].Path != "src/foo.ts" {
		t.Fatalf("post-settle pathRefs: got %#v, want single src/foo.ts", meta.PathRefs)
	}
}

// TestStreamingTextSettleFlushEmitsForNewPath is the complementary case
// to TestStreamingTextDedupeSurvivesSettle. The buffered settle-time
// delta introduces a NEW validated path. Settle's flushStreamingItem
// drains the buffer, enrichStreamingPathRefsAndEmit runs, merged
// pathRefs JSON differs from the cache, so the dedupe legitimately
// emits a second meta event. This proves the cache state (not just the
// defer order) is what gates dedupe — a regression that always-emitted
// or always-suppressed would fail this assertion.
func TestStreamingTextSettleFlushEmitsForNewPath(t *testing.T) {
	wsRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(wsRoot, "src"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wsRoot, "src", "foo.ts"), nil, 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wsRoot, "src", "bar.ts"), nil, 0o644); err != nil {
		t.Fatalf("seed bar file: %v", err)
	}

	router, st, emissions := newTestRouter(t)
	ensureTriageProject(t, st)
	now := time.Now().UnixMilli()
	if err := st.CreateThread(store.Thread{
		ID:            "t1",
		ProjectID:     triageTestProjectID,
		Title:         "live-pathrefs-settle-newpath",
		Provider:      "claude",
		WorkspacePath: wsRoot,
		CreatedAt:     now,
		UpdatedAt:     now,
	}); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	// Setup: deltas 1+2 mid-stream-emit foo.ts (count=1, cache populated).
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTextDelta,
		ThreadID:  "t1",
		Content:   "see src/foo.ts and ",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("delta 1: %v", err)
	}
	padding := strings.Repeat("x", streamPersistByteThreshold+1)
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTextDelta,
		ThreadID:  "t1",
		Content:   padding,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("delta 2: %v", err)
	}
	if got := filterItemEventMetas(*emissions); len(got) != 1 {
		t.Fatalf("expected 1 mid-stream meta after delta 2, got %d", len(got))
	}

	// Delta 3 — small, BELOW threshold, introduces src/bar.ts. The
	// buffer holds it until settle's flushStreamingItem drains it.
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTextDelta,
		ThreadID:  "t1",
		Content:   " also src/bar.ts here",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("delta 3: %v", err)
	}

	// Settle. enrichStreamingPathRefsAndEmit fires against the row
	// whose summary now includes the new bar.ts ref. Merged pathRefs
	// JSON differs from the cache (now contains both refs), so the
	// dedupe legitimately emits — count becomes 2.
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventContentBlockStop,
		ThreadID:  "t1",
		Meta:      json.RawMessage(`{"blockType":"text"}`),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("content block stop: %v", err)
	}
	router.WaitForPendingSettles()

	metas := filterItemEventMetas(*emissions)
	if len(metas) != 2 {
		t.Fatalf("expected settle-flush emit for new path (count 2); got %d: %+v", len(metas), metas)
	}
	// Sanity check the new meta payload carries both paths.
	var metaPayload struct {
		PathRefs []pathlinks.PathRef `json:"pathRefs"`
	}
	if err := json.Unmarshal([]byte(metas[1].Meta), &metaPayload); err != nil {
		t.Fatalf("unmarshal second meta %q: %v", metas[1].Meta, err)
	}
	paths := make(map[string]bool, len(metaPayload.PathRefs))
	for _, r := range metaPayload.PathRefs {
		paths[r.Path] = true
	}
	if !paths["src/foo.ts"] || !paths["src/bar.ts"] {
		t.Fatalf("expected settle-flush meta to carry both src/foo.ts and src/bar.ts; got %#v", metaPayload.PathRefs)
	}
}

// TestStreamingTextLiveCacheClearedOnSettle confirms the per-row
// cache entry doesn't outlive the row. After settle, streamingPathRefsLast
// for that key must be empty (cleared by defer in doSettleStreamingText)
// so a subsequent re-stream under the same itemID — e.g., a Claude
// system.init resend after interrupt — sees a fresh dedupe state.
func TestStreamingTextLiveCacheClearedOnSettle(t *testing.T) {
	wsRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(wsRoot, "src"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wsRoot, "src", "foo.ts"), nil, 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	router, st, _ := newTestRouter(t)
	ensureTriageProject(t, st)
	now := time.Now().UnixMilli()
	if err := st.CreateThread(store.Thread{
		ID:            "t1",
		ProjectID:     triageTestProjectID,
		Title:         "live-pathrefs-clear",
		Provider:      "claude",
		WorkspacePath: wsRoot,
		CreatedAt:     now,
		UpdatedAt:     now,
	}); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTextDelta,
		ThreadID:  "t1",
		Content:   "see src/foo.ts and ",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("delta 1: %v", err)
	}
	padding := strings.Repeat("x", streamPersistByteThreshold+1)
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTextDelta,
		ThreadID:  "t1",
		Content:   padding,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("delta 2: %v", err)
	}

	// Sanity: cache populated mid-stream.
	if got := liveCacheSize(router); got == 0 {
		t.Fatalf("expected streamingPathRefsLast to be populated mid-stream; got 0 entries")
	}

	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventContentBlockStop,
		ThreadID:  "t1",
		Meta:      json.RawMessage(`{"blockType":"text"}`),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("content block stop: %v", err)
	}
	router.WaitForPendingSettles()

	if got := liveCacheSize(router); got != 0 {
		t.Fatalf("expected streamingPathRefsLast empty after settle; got %d entries", got)
	}
}

// TestStreamingTextLiveCacheClearedOnCleanupThread covers the broad
// per-thread sweep. Any cache entries left behind by a stream that
// did not reach settle (e.g., process crash, thread teardown) must
// be reclaimed when the thread is cleaned up.
func TestStreamingTextLiveCacheClearedOnCleanupThread(t *testing.T) {
	wsRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(wsRoot, "src"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wsRoot, "src", "foo.ts"), nil, 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	router, st, _ := newTestRouter(t)
	ensureTriageProject(t, st)
	now := time.Now().UnixMilli()
	if err := st.CreateThread(store.Thread{
		ID:            "t1",
		ProjectID:     triageTestProjectID,
		Title:         "live-pathrefs-cleanup",
		Provider:      "claude",
		WorkspacePath: wsRoot,
		CreatedAt:     now,
		UpdatedAt:     now,
	}); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTextDelta,
		ThreadID:  "t1",
		Content:   "see src/foo.ts and ",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("delta 1: %v", err)
	}
	padding := strings.Repeat("x", streamPersistByteThreshold+1)
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTextDelta,
		ThreadID:  "t1",
		Content:   padding,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("delta 2: %v", err)
	}
	if got := liveCacheSize(router); got == 0 {
		t.Fatalf("expected streamingPathRefsLast to be populated mid-stream")
	}

	router.CleanupThread("t1")
	if got := liveCacheSize(router); got != 0 {
		t.Fatalf("expected CleanupThread to drop streamingPathRefsLast entries; got %d", got)
	}
}

// TestStreamingTextSkipsMidStreamEmitWithoutWorkspace mirrors the
// settle-path skip behavior for the live emit hook: a thread with
// no workspace can't validate paths, so the byte-threshold flush
// must still persist the summary without emitting an empty pathRefs
// meta. Keeps the live path safe under partial state.
func TestStreamingTextSkipsMidStreamEmitWithoutWorkspace(t *testing.T) {
	router, st, emissions := newTestRouter(t)
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
		Content:   "see src/foo.ts and ",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("delta 1: %v", err)
	}
	padding := strings.Repeat("x", streamPersistByteThreshold+1)
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTextDelta,
		ThreadID:  "t-no-ws",
		Content:   padding,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("delta 2: %v", err)
	}

	if got := filterItemEventMetas(*emissions); len(got) != 0 {
		t.Fatalf("expected 0 mid-stream meta events without a workspace; got %d: %+v", len(got), got)
	}
}

// liveCacheSize reads streamingPathRefsLast under the router's lock.
// Tests must NOT touch the map directly because the live emit path
// writes to it from the same goroutine doing handle() calls.
func liveCacheSize(r *Router) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.streamingPathRefsLast)
}

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/highlight"
	"agent-overflow/internal/store"
)

const diffSeedTestPatch = "diff --git a/src/app.py b/src/app.py\n" +
	"--- a/src/app.py\n" +
	"+++ b/src/app.py\n" +
	"@@ -1,1 +1,2 @@\n" +
	" def f():\n" +
	"+    pass"

func TestComputePatchSpanSeeds(t *testing.T) {
	a := &App{}
	other := "diff --git a/notes.txt b/notes.txt\n" +
		"--- a/notes.txt\n" +
		"+++ b/notes.txt\n" +
		"@@ -1 +1 @@\n" +
		"-old\n" +
		"+new"
	seeds := a.computePatchSpanSeeds(diffSeedTestPatch+"\n"+other, nil)
	if len(seeds) != 2 {
		t.Fatalf("expected 2 seeds, got %#v", seeds)
	}
	if seeds[0].Path != "src/app.py" || seeds[1].Path != "notes.txt" {
		t.Fatalf("seed paths wrong: %q, %q", seeds[0].Path, seeds[1].Path)
	}
	if seeds[0].ContentKey != highlight.FrontendContentKey(diffSeedTestPatch) {
		t.Fatalf("content key mismatch: %q", seeds[0].ContentKey)
	}
	if len(seeds[0].Lines) != strings.Count(diffSeedTestPatch, "\n")+1 {
		t.Fatalf("seed lines must align 1:1 with patch lines, got %d", len(seeds[0].Lines))
	}
	// Unknown languages still seed (all-plain is the authoritative
	// answer that stops a frontend re-request).
	if seeds[1].ContentKey != highlight.FrontendContentKey(other) {
		t.Fatalf("unknown-lang seed missing: %#v", seeds[1])
	}

	// An over-cap file is skipped while its siblings still seed.
	big := "diff --git a/big.py b/big.py\n--- a/big.py\n+++ b/big.py\n@@ -0,0 +1 @@\n+" +
		strings.Repeat("x", diffSeedMaxFileBytes)
	seeds = a.computePatchSpanSeeds(big+"\n"+diffSeedTestPatch, nil)
	if len(seeds) != 1 || seeds[0].Path != "src/app.py" {
		t.Fatalf("expected only the small file to seed, got %#v", seeds)
	}

	// Invalid UTF-8 never seeds: the wire's U+FFFD replacement would
	// make the frontend key MATCH while the spans cover the original
	// byte lengths — the one divergence that could misalign colors.
	invalid := "diff --git a/bad.py b/bad.py\n--- a/bad.py\n+++ b/bad.py\n@@ -0,0 +1 @@\n+x\xff\xfe"
	seeds = a.computePatchSpanSeeds(invalid+"\n"+diffSeedTestPatch, nil)
	if len(seeds) != 1 || seeds[0].Path != "src/app.py" {
		t.Fatalf("expected the invalid-UTF-8 file to be skipped, got %#v", seeds)
	}

	// A file too big to ever highlight (over the per-file cap AND the
	// aggregate budget, under the scan cap) must be skipped BEFORE the
	// aggregate budget check — tripping the budget break on a file the
	// loop would never highlight starves valid later siblings.
	huge := "diff --git a/huge.py b/huge.py\n--- a/huge.py\n+++ b/huge.py\n@@ -0,0 +1 @@\n+" +
		strings.Repeat("x", diffSeedMaxTotalBytes+1)
	seeds = a.computePatchSpanSeeds(huge+"\n"+diffSeedTestPatch, nil)
	if len(seeds) != 1 || seeds[0].Path != "src/app.py" {
		t.Fatalf("skipped file must not consume aggregate budget, got %#v", seeds)
	}

	if got := a.computePatchSpanSeeds("", nil); got != nil {
		t.Fatalf("empty patch must not seed, got %#v", got)
	}
}

func collectDiffSeeds(a *App) (*[]HighlightDiffSeedEvent, *sync.Mutex) {
	events := &[]HighlightDiffSeedEvent{}
	mu := &sync.Mutex{}
	a.testEmitHook = func(name string, data any) {
		if name != "highlight:diff_seed" {
			return
		}
		evt, ok := data.(HighlightDiffSeedEvent)
		if !ok {
			return
		}
		mu.Lock()
		*events = append(*events, evt)
		mu.Unlock()
	}
	return events, mu
}

// diffSpanItemIndex keeps insertDiffSpanPayload's item indexes unique
// within a thread across calls.
var diffSpanItemIndex atomic.Int32

// insertDiffSpanPayload seeds one item+payload pair the observer /
// payload-load tests can write spans against.
func insertDiffSpanPayload(t *testing.T, app *App, threadID, itemID, payloadID, kind, data string) {
	t.Helper()
	now := time.Now().UnixMilli()
	if err := app.store.InsertItemWithPayload(store.Item{
		ID:        itemID,
		ThreadID:  threadID,
		ItemIndex: int(diffSpanItemIndex.Add(1)),
		Kind:      "tool_call",
		Role:      "assistant",
		Status:    "completed",
		Summary:   "diff",
		PayloadID: payloadID,
		CreatedAt: now,
		UpdatedAt: now,
	}, store.Payload{
		ID:        payloadID,
		Kind:      kind,
		Data:      []byte(data),
		CreatedAt: now,
	}); err != nil {
		t.Fatalf("InsertItemWithPayload(%s) error = %v", payloadID, err)
	}
}

// waitForPayloadSpans polls until the async persist worker has written
// the payload's spans column (and, via the deferred counter decrement
// ordering, until the seed-push decision has also been made).
func waitForPayloadSpans(t *testing.T, app *App, payloadID string) PersistedPatchSpans {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		blob, err := app.store.GetPayloadSpans(payloadID)
		if err != nil {
			t.Fatalf("GetPayloadSpans() error = %v", err)
		}
		if blob != "" && app.diffSeedWorkers.Load() == 0 {
			var spans PersistedPatchSpans
			if err := json.Unmarshal([]byte(blob), &spans); err != nil {
				t.Fatalf("unmarshal spans blob %q: %v", blob, err)
			}
			return spans
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for persisted spans on %s", payloadID)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestObserveDiffPayloadPersistedWritesColumnsAndPushes(t *testing.T) {
	app := newTestAppWithStore(t)
	app.remoteClientProbeFn = func() bool { return true }
	events, mu := collectDiffSeeds(app)

	thread := testThread("thread-diff-spans")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	// The preview is a truncation of the full patch — distinct content,
	// so the two blobs must carry distinct contentKeys.
	preview := "diff --git a/src/app.py b/src/app.py\n" +
		"--- a/src/app.py\n" +
		"+++ b/src/app.py\n" +
		"@@ -1,1 +1,2 @@\n" +
		" def f():"
	insertDiffSpanPayload(t, app, thread.ID, "item-1", "payload-1", "tool_result", diffSeedTestPatch)

	app.observeDiffPayloadPersisted(thread.ID, "payload-1", []string{preview}, diffSeedTestPatch)

	full := waitForPayloadSpans(t, app, "payload-1")
	if full.Version != highlight.SchemaVersion() {
		t.Fatalf("spans version = %q, want %q", full.Version, highlight.SchemaVersion())
	}
	if len(full.Files) != 1 || full.Files[0].ContentKey != highlight.FrontendContentKey(diffSeedTestPatch) {
		t.Fatalf("full spans must key the full patch: %#v", full.Files)
	}

	// preview_spans rides the item join as Item.PayloadPreviewSpans.
	item, found, err := app.store.GetThreadItem(thread.ID, "item-1")
	if err != nil || !found {
		t.Fatalf("GetThreadItem() = %v found=%v", err, found)
	}
	var previewSpans PersistedPatchSpans
	if err := json.Unmarshal([]byte(item.PayloadPreviewSpans), &previewSpans); err != nil {
		t.Fatalf("unmarshal item preview spans %q: %v", item.PayloadPreviewSpans, err)
	}
	if previewSpans.Version != highlight.SchemaVersion() ||
		len(previewSpans.Files) != 1 ||
		previewSpans.Files[0].ContentKey != highlight.FrontendContentKey(preview) {
		t.Fatalf("preview spans must key the preview text: %#v", previewSpans)
	}

	// The live remote push carries the preview seeds.
	mu.Lock()
	defer mu.Unlock()
	if len(*events) != 1 {
		t.Fatalf("expected one highlight:diff_seed push, got %#v", *events)
	}
	evt := (*events)[0]
	if evt.ThreadID != thread.ID || len(evt.Files) != 1 ||
		evt.Files[0].ContentKey != highlight.FrontendContentKey(preview) {
		t.Fatalf("diff seed push wrong: %#v", evt)
	}
}

func TestObserveDiffPayloadPersistedWithoutRemoteStillPersists(t *testing.T) {
	app := newTestAppWithStore(t)
	app.remoteClientProbeFn = func() bool { return false }
	events, mu := collectDiffSeeds(app)

	thread := testThread("thread-diff-local")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	insertDiffSpanPayload(t, app, thread.ID, "item-1", "payload-1", "diff", diffSeedTestPatch)

	// Diff-kind payloads notify with no previews: the full-data spans
	// persist for everyone even with no remote client attached.
	app.observeDiffPayloadPersisted(thread.ID, "payload-1", nil, diffSeedTestPatch)

	full := waitForPayloadSpans(t, app, "payload-1")
	if len(full.Files) != 1 || full.Files[0].Path != "src/app.py" {
		t.Fatalf("full spans wrong: %#v", full.Files)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(*events) != 0 {
		t.Fatalf("expected no push without a remote client, got %#v", *events)
	}
}

func TestObserveDiffPayloadPersistedDropsPastWorkerCap(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-diff-cap")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	insertDiffSpanPayload(t, app, thread.ID, "item-1", "payload-1", "diff", diffSeedTestPatch)

	// Saturate the worker budget; the observation must drop instead of
	// queueing a goroutine behind the parse semaphore. The payload then
	// simply never gets persisted spans — the RPC path covers it.
	app.diffSeedWorkers.Store(diffSeedMaxWorkers)
	app.observeDiffPayloadPersisted(thread.ID, "payload-1", nil, diffSeedTestPatch)
	time.Sleep(50 * time.Millisecond)

	blob, err := app.store.GetPayloadSpans("payload-1")
	if err != nil {
		t.Fatalf("GetPayloadSpans() error = %v", err)
	}
	if blob != "" {
		t.Fatalf("saturated observer must not persist spans, got %q", blob)
	}
	if load := app.diffSeedWorkers.Load(); load != diffSeedMaxWorkers {
		t.Fatalf("drop must restore the counter, got %d", load)
	}
}

func TestObserveDiffPayloadPersistedBoundsAggregateInput(t *testing.T) {
	app := newTestAppWithStore(t)
	app.remoteClientProbeFn = func() bool { return true }
	events, mu := collectDiffSeeds(app)

	thread := testThread("thread-diff-budget")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	insertDiffSpanPayload(t, app, thread.ID, "item-1", "payload-1", "tool_result", diffSeedTestPatch)

	// Two previews whose combined size crosses the aggregate budget:
	// the first seeds, the second is dropped BEFORE compute — the
	// budget bounds input accepted, not work started.
	overCap := "diff --git a/big.txt b/big.txt\n--- a/big.txt\n+++ b/big.txt\n@@ -0,0 +1 @@\n+" +
		strings.Repeat("x", diffSeedMaxTotalBytes)
	app.observeDiffPayloadPersisted(thread.ID, "payload-1", []string{diffSeedTestPatch, overCap}, diffSeedTestPatch)

	waitForPayloadSpans(t, app, "payload-1")
	mu.Lock()
	defer mu.Unlock()
	if len(*events) != 1 {
		t.Fatalf("expected one push, got %#v", *events)
	}
	evt := (*events)[0]
	if len(evt.Files) != 1 || evt.Files[0].Path != "src/app.py" {
		t.Fatalf("expected only the first preview to seed, got %#v", evt.Files)
	}
}

func TestObserveDiffPayloadPersistedSkipsOversizedLeadingPreview(t *testing.T) {
	app := newTestAppWithStore(t)
	app.remoteClientProbeFn = func() bool { return true }
	events, mu := collectDiffSeeds(app)

	thread := testThread("thread-diff-lead")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	insertDiffSpanPayload(t, app, thread.ID, "item-1", "payload-1", "tool_result", diffSeedTestPatch)

	// An over-budget preview in FIRST position: it can never seed (the
	// per-file cap would skip it inside the compute), so it must not
	// trip the aggregate-budget break and starve the valid preview
	// behind it.
	overCap := "diff --git a/big.txt b/big.txt\n--- a/big.txt\n+++ b/big.txt\n@@ -0,0 +1 @@\n+" +
		strings.Repeat("x", diffSeedMaxTotalBytes)
	app.observeDiffPayloadPersisted(thread.ID, "payload-1", []string{overCap, diffSeedTestPatch}, diffSeedTestPatch)

	waitForPayloadSpans(t, app, "payload-1")
	mu.Lock()
	defer mu.Unlock()
	if len(*events) != 1 {
		t.Fatalf("expected one push, got %#v", *events)
	}
	evt := (*events)[0]
	if len(evt.Files) != 1 || evt.Files[0].Path != "src/app.py" {
		t.Fatalf("oversized leading preview must not starve later ones, got %#v", evt.Files)
	}
}

func TestObserveDiffPayloadPersistedDeletedPayloadNeverPushes(t *testing.T) {
	app := newTestAppWithStore(t)
	app.remoteClientProbeFn = func() bool { return true }
	events, mu := collectDiffSeeds(app)

	thread := testThread("thread-diff-deleted")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	// The payload row never exists (stands in for thread deletion racing
	// the worker): the span write reports no row, and the seed push must
	// be suppressed too — the frontend's cleanup for the thread may
	// already have run, and a late seed would re-register cache entries
	// for it.
	app.observeDiffPayloadPersisted(thread.ID, "payload-gone", []string{diffSeedTestPatch}, diffSeedTestPatch)

	deadline := time.Now().Add(10 * time.Second)
	for app.diffSeedWorkers.Load() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the worker to finish")
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(*events) != 0 {
		t.Fatalf("deleted payload must not push seeds, got %#v", *events)
	}
}

func TestGetPayloadDataServesPersistedPatchSpans(t *testing.T) {
	app := newTestAppWithStore(t)

	thread := testThread("thread-patch-spans")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	insertDiffSpanPayload(t, app, thread.ID, "item-diff", "payload-diff", "tool_result", diffSeedTestPatch)
	insertDiffSpanPayload(t, app, thread.ID, "item-cmd", "payload-cmd", "command_output", diffSeedTestPatch)
	insertDiffSpanPayload(t, app, thread.ID, "item-stale", "payload-stale", "diff", diffSeedTestPatch)
	insertDiffSpanPayload(t, app, thread.ID, "item-none", "payload-none", "diff", diffSeedTestPatch)

	// Simulate the persist worker: current-version spans on payload-diff,
	// a stale-schema blob on payload-stale, nothing on payload-none.
	seeds := app.computePatchSpanSeeds(diffSeedTestPatch, nil)
	if len(seeds) != 1 {
		t.Fatalf("fixture seeds wrong: %#v", seeds)
	}
	current := marshalPersistedPatchSpans(seeds)
	if err := app.store.UpdatePayloadSpans("payload-diff", "", current); err != nil {
		t.Fatalf("UpdatePayloadSpans(payload-diff) error = %v", err)
	}
	stale, err := json.Marshal(PersistedPatchSpans{Version: "stale-schema", Files: seeds})
	if err != nil {
		t.Fatalf("marshal stale blob: %v", err)
	}
	if err := app.store.UpdatePayloadSpans("payload-stale", "", string(stale)); err != nil {
		t.Fatalf("UpdatePayloadSpans(payload-stale) error = %v", err)
	}
	// Non-diff kinds never attach, even with a blob present.
	if err := app.store.UpdatePayloadSpans("payload-cmd", "", current); err != nil {
		t.Fatalf("UpdatePayloadSpans(payload-cmd) error = %v", err)
	}

	content, err := app.GetPayloadData(thread.ID, "payload-diff")
	if err != nil {
		t.Fatalf("GetPayloadData() error = %v", err)
	}
	if content.Data != diffSeedTestPatch {
		t.Fatalf("payload data changed: %q", content.Data)
	}
	if len(content.PatchSpans) != 1 || content.PatchSpans[0].Path != "src/app.py" ||
		content.PatchSpans[0].ContentKey != highlight.FrontendContentKey(diffSeedTestPatch) {
		t.Fatalf("payload patch spans wrong: %#v", content.PatchSpans)
	}

	// A blob stamped by another build's schema is dropped server-side.
	content, err = app.GetPayloadData(thread.ID, "payload-stale")
	if err != nil {
		t.Fatalf("GetPayloadData(stale) error = %v", err)
	}
	if content.PatchSpans != nil {
		t.Fatalf("stale-schema spans must not attach, got %#v", content.PatchSpans)
	}

	// No blob → no spans (the frontend's RPC path covers).
	content, err = app.GetPayloadData(thread.ID, "payload-none")
	if err != nil {
		t.Fatalf("GetPayloadData(none) error = %v", err)
	}
	if content.PatchSpans != nil {
		t.Fatalf("absent blob must not attach spans, got %#v", content.PatchSpans)
	}

	// Non-diff payload kinds never attach spans.
	content, err = app.GetPayloadData(thread.ID, "payload-cmd")
	if err != nil {
		t.Fatalf("GetPayloadData(cmd) error = %v", err)
	}
	if content.PatchSpans != nil {
		t.Fatalf("expected no spans for command_output kind, got %#v", content.PatchSpans)
	}

	// Preview attaches the same persisted blob. The served prefix
	// truncates the file here, so the spans won't match the truncated
	// text client-side — content addressing makes them inert, and the
	// one boundary file falls back to the RPC path.
	preview, err := app.GetPayloadPreview(thread.ID, "payload-diff", 64)
	if err != nil {
		t.Fatalf("GetPayloadPreview() error = %v", err)
	}
	if len(preview.Data) > 64 || len(preview.PatchSpans) != 1 {
		t.Fatalf("preview spans wrong: data=%d spans=%#v", len(preview.Data), preview.PatchSpans)
	}
	if preview.PatchSpans[0].ContentKey != highlight.FrontendContentKey(diffSeedTestPatch) {
		t.Fatalf("preview must attach the persisted full-content blob: %#v", preview.PatchSpans[0])
	}
}

func TestCapPatchSpanSeedBytes(t *testing.T) {
	seed := func(path string, lines int) PatchSpanSeed {
		s := PatchSpanSeed{Path: path}
		for i := 0; i < lines; i++ {
			s.Lines = append(s.Lines, highlight.EncodedLine{})
		}
		return s
	}
	// Budget fits the first and third seeds; the oversized middle one
	// is skipped without breaking the walk.
	small, big := seed("a", 1), seed("b", 100)
	got := capPatchSpanSeedBytes([]PatchSpanSeed{small, big, seed("c", 1)}, 3*8)
	if len(got) != 2 || got[0].Path != "a" || got[1].Path != "c" {
		t.Fatalf("cap must skip-not-break: %#v", got)
	}
	if capPatchSpanSeedBytes([]PatchSpanSeed{big}, 8) != nil {
		t.Fatal("all-over-budget must return nil")
	}
}

func TestComputePatchSpanSeedsPrimed(t *testing.T) {
	a := &App{}
	matching := "def f():\n    pass\n"

	primed := a.computePatchSpanSeeds(diffSeedTestPatch, func(path string) string {
		if path != "src/app.py" {
			t.Fatalf("prime resolver called with %q", path)
		}
		return matching
	})
	if len(primed) != 1 || !primed[0].Primed {
		t.Fatalf("matching content must prime, got %+v", primed)
	}

	// Drifted content (a later edit landed before the worker read the
	// file) degrades to unprimed spans — never wrong colors.
	drifted := a.computePatchSpanSeeds(diffSeedTestPatch, func(string) string {
		return "def f():\n    something_else\n"
	})
	if len(drifted) != 1 || drifted[0].Primed {
		t.Fatalf("drifted content must not prime, got %+v", drifted)
	}

	unresolved := a.computePatchSpanSeeds(diffSeedTestPatch, func(string) string { return "" })
	if len(unresolved) != 1 || unresolved[0].Primed {
		t.Fatalf("unresolvable content must not prime, got %+v", unresolved)
	}
}

func TestObserveDiffPayloadPersistedCapturesEditFileSnapshots(t *testing.T) {
	app := newTestAppWithStore(t)
	workspace := t.TempDir()
	thread := testThread("thread-snapshots")
	thread.WorkspacePath = workspace
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	if err := os.MkdirAll(filepath.Join(workspace, "src"), 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	// src/app.py matches its patch section's new side; notes.txt drifted
	// (a later edit beat the worker to the file).
	if err := os.WriteFile(filepath.Join(workspace, "src", "app.py"), []byte("def f():\n    pass\n"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "notes.txt"), []byte("already drifted\n"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	notesPatch := "diff --git a/notes.txt b/notes.txt\n" +
		"--- a/notes.txt\n" +
		"+++ b/notes.txt\n" +
		"@@ -1 +1 @@\n" +
		"-old\n" +
		"+new"
	patch := diffSeedTestPatch + "\n" + notesPatch
	insertDiffSpanPayload(t, app, thread.ID, "item-1", "payload-1", "tool_result", patch)

	app.observeDiffPayloadPersisted(thread.ID, "payload-1", nil, patch)
	waitForPayloadSpans(t, app, "payload-1")

	// The verified file snapshots with the post-edit workspace content.
	content, found, err := app.store.GetEditFileSnapshot(thread.ID, "payload-1", "src/app.py")
	if err != nil || !found {
		t.Fatalf("GetEditFileSnapshot() = %v found=%v", err, found)
	}
	if content != "def f():\n    pass\n" {
		t.Fatalf("snapshot content = %q", content)
	}
	// The drifted file must NOT snapshot — a wrong snapshot would serve
	// wrong gap lines forever; absence degrades to workspace verify.
	if _, found, err := app.store.GetEditFileSnapshot(thread.ID, "payload-1", "notes.txt"); err != nil || found {
		t.Fatalf("drifted file snapshot: err=%v found=%v, want nil/false", err, found)
	}
}

func TestWorkspaceFilePrimer(t *testing.T) {
	app := newTestAppWithStore(t)
	workspace := t.TempDir()
	thread := testThread("thread-primer")
	thread.WorkspacePath = workspace
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	if err := os.MkdirAll(filepath.Join(workspace, "src"), 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "src", "app.py"), []byte("def f():\n    pass\n"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	prime := app.workspaceFilePrimer(thread.ID)
	if prime == nil {
		t.Fatal("workspaceFilePrimer() = nil for a resolvable thread")
	}
	if got := prime("src/app.py"); got != "def f():\n    pass\n" {
		t.Fatalf("prime(src/app.py) = %q", got)
	}
	// Workspace-escaping and absolute paths resolve to "" (unprimed),
	// never to an out-of-workspace read.
	if got := prime("../outside.py"); got != "" {
		t.Fatalf("prime(../outside.py) = %q, want empty", got)
	}
	if got := prime("/etc/hostname"); got != "" {
		t.Fatalf("prime(/etc/hostname) = %q, want empty", got)
	}
	if got := prime("src/missing.py"); got != "" {
		t.Fatalf("prime(missing) = %q, want empty", got)
	}

	if app.workspaceFilePrimer("no-such-thread") != nil {
		t.Fatal("workspaceFilePrimer() must be nil for an unknown thread")
	}
}

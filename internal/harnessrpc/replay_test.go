package harnessrpc

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/harness"
	"agent-overflow/internal/observability/replay"
	"agent-overflow/internal/store"
)

// logHarnessEvent pushes one record through the event-log manager, the
// same path emitWithReplay feeds in production.
func logHarnessEvent(t *testing.T, host *testHost, ts time.Time, threadID, kind string, data any) {
	t.Helper()
	rec, err := replay.NewRecord(ts, threadID, kind, data)
	if err != nil {
		t.Fatalf("NewRecord: %v", err)
	}
	if !host.replay.Enqueue(rec) {
		t.Fatal("event log dropped a record")
	}
}

func TestHarnessRecordAndReplayBundleRoundTrip(t *testing.T) {
	h, host := newHarnessTestHost(t)
	const threadID = "thread-bundle-1"
	base := time.Now()

	// Pre-recording noise must stay out of the bundle.
	logHarnessEvent(t, host, base, threadID, "provider:item_event", map[string]any{"n": 0})

	rec, err := h.HarnessRecordStart("flicker-repro", threadID)
	if err != nil {
		t.Fatalf("HarnessRecordStart: %v", err)
	}
	if rec.Name != "flicker-repro" || rec.ThreadID != threadID {
		t.Fatalf("recording = %+v", rec)
	}

	// Concurrent second recording is refused.
	if _, err := h.HarnessRecordStart("another", threadID); err == nil {
		t.Fatal("second HarnessRecordStart succeeded while one was active")
	}

	logHarnessEvent(t, host, base.Add(10*time.Millisecond), threadID, "provider:item_event", map[string]any{"n": 1})
	logHarnessEvent(t, host, base.Add(20*time.Millisecond), threadID, "provider:turn_completed", map[string]any{"n": 2})

	meta, err := h.HarnessRecordStop()
	if err != nil {
		t.Fatalf("HarnessRecordStop: %v", err)
	}
	if meta.EventCount != 2 || meta.SnapshotAt != "start" {
		t.Fatalf("meta = %+v, want 2 events snapshotted at start", meta)
	}
	bundleDir := filepath.Join(h.config.DataDir, "bundles", "flicker-repro")
	for _, f := range []string{"events.jsonl", "db.snapshot", "meta.json"} {
		if _, err := os.Stat(filepath.Join(bundleDir, f)); err != nil {
			t.Fatalf("bundle missing %s: %v", f, err)
		}
	}

	bundles, err := h.HarnessListBundles()
	if err != nil || len(bundles) != 1 || bundles[0].Name != "flicker-repro" {
		t.Fatalf("HarnessListBundles = %+v, %v", bundles, err)
	}

	// Replay the bundle: the two recorded events (and only those) come
	// back out through app.emit, plus harness:replay progress events.
	var mu sync.Mutex
	var replayed []string
	host.emit = func(channel eventchan.Channel, data any) {
		name := channel.String()
		if name != "provider:item_event" && name != "provider:turn_completed" {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		replayed = append(replayed, fmt.Sprintf("%s:%s", name, data))
	}

	if _, err := h.HarnessReplayBundle("flicker-repro", harness.ReplayOptions{}); err != nil {
		t.Fatalf("HarnessReplayBundle: %v", err)
	}
	if host.published == nil {
		t.Fatal("HarnessReplayBundle restored the store without publishing its new identity")
	}
	deadline := time.Now().Add(5 * time.Second)
	for h.HarnessReplayStatus().State != "done" {
		if time.Now().After(deadline) {
			t.Fatalf("replay never finished: %+v", h.HarnessReplayStatus())
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(replayed) != 2 {
		t.Fatalf("replayed = %v, want the 2 recorded events (pre-recording noise excluded)", replayed)
	}
	if want := `provider:item_event:{"n":1}`; replayed[0] != want {
		t.Fatalf("first replayed event = %q, want %q", replayed[0], want)
	}
}

func TestHarnessRecordStopWithoutStart(t *testing.T) {
	h, _ := newHarnessTestHost(t)
	if _, err := h.HarnessRecordStop(); err == nil {
		t.Fatal("HarnessRecordStop succeeded with no active recording")
	}
}

func TestHarnessRecordStopWithNoEvents(t *testing.T) {
	h, _ := newHarnessTestHost(t)
	if _, err := h.HarnessRecordStart("empty", "thread-x"); err != nil {
		t.Fatalf("HarnessRecordStart: %v", err)
	}
	if _, err := h.HarnessRecordStop(); err == nil {
		t.Fatal("HarnessRecordStop produced a bundle with zero events")
	}
	// The failed capture cleared the slot AND removed the half-built
	// bundle dir: the same name is immediately reusable.
	if _, err := os.Stat(filepath.Join(h.config.DataDir, "bundles", "empty")); !os.IsNotExist(err) {
		t.Fatalf("failed stop left the bundle dir behind (stat err %v)", err)
	}
	if _, err := h.HarnessRecordStart("empty", "thread-x"); err != nil {
		t.Fatalf("HarnessRecordStart reusing the discarded name: %v", err)
	}
}

func TestHarnessRecordStartValidation(t *testing.T) {
	h, host := newHarnessTestHost(t)
	for _, name := range []string{"", "a/b", `a\b`, ".", ".."} {
		if _, err := h.HarnessRecordStart(name, "t1"); err == nil {
			t.Fatalf("HarnessRecordStart accepted bundle name %q", name)
		}
	}
	if _, err := h.HarnessRecordStart("ok", ""); err == nil {
		t.Fatal("HarnessRecordStart accepted an empty threadId")
	}
	// Duplicate bundle names are refused (the dir already exists after
	// a successful capture).
	if _, err := h.HarnessRecordStart("dup", "t1"); err != nil {
		t.Fatalf("HarnessRecordStart: %v", err)
	}
	logHarnessEvent(t, host, time.Now(), "t1", "provider:item_event", map[string]any{"n": 1})
	if _, err := h.HarnessRecordStop(); err != nil {
		t.Fatalf("HarnessRecordStop: %v", err)
	}
	if _, err := h.HarnessRecordStart("dup", "t1"); err == nil {
		t.Fatal("HarnessRecordStart reused an existing bundle name")
	}
}

// TestHarnessRecordStartRefusesActiveTurn pins the exact-boundary
// contract: with a turn in flight an event could land in both the DB
// snapshot and the event slice (or neither), so RecordStart refuses.
func TestHarnessRecordStartRefusesActiveTurn(t *testing.T) {
	h, host := newHarnessTestHost(t)
	seedHarnessThread(t, host.store, "thread-live")
	if err := host.store.InsertTurn(store.Turn{
		TurnID: "thread-live:1", ThreadID: "thread-live", TurnIndex: 1,
		StartedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("InsertTurn: %v", err)
	}

	if _, err := h.HarnessRecordStart("mid-turn", "thread-live"); err == nil {
		t.Fatal("HarnessRecordStart accepted a thread with a turn in flight")
	}

	if err := host.store.UpdateTurnCompleted("thread-live:1", time.Now().UnixMilli(), "end_turn", "", "", ""); err != nil {
		t.Fatalf("UpdateTurnCompleted: %v", err)
	}
	if _, err := h.HarnessRecordStart("post-turn", "thread-live"); err != nil {
		t.Fatalf("HarnessRecordStart on an idle thread: %v", err)
	}
}

// TestHarnessRecordStopDetectsRotation: a rotated event log that grows
// back past the start offset must fail the capture by file identity —
// a size check alone would silently slice the replacement file.
func TestHarnessRecordStopDetectsRotation(t *testing.T) {
	h, host := newHarnessTestHost(t)
	const threadID = "thread-rotate"
	logHarnessEvent(t, host, time.Now(), threadID, "provider:item_event", map[string]any{"n": 0})
	if _, err := h.HarnessRecordStart("rotated", threadID); err != nil {
		t.Fatalf("HarnessRecordStart: %v", err)
	}
	logHarnessEvent(t, host, time.Now(), threadID, "provider:item_event", map[string]any{"n": 1})

	// Simulate the writer's size-based rotation: current file becomes
	// .1, a replacement appears at the original path — larger than the
	// recorded start offset so the old size check would pass.
	logPath := h.eventLogPath(threadID)
	waitForCond(t, "event log flushed", func() bool {
		info, err := os.Stat(logPath)
		return err == nil && info.Size() > 0
	})
	if err := os.Rename(logPath, logPath+".1"); err != nil {
		t.Fatalf("simulate rotation: %v", err)
	}
	replacement := make([]byte, 4096)
	for i := range replacement {
		replacement[i] = '\n'
	}
	if err := os.WriteFile(logPath, replacement, 0o600); err != nil {
		t.Fatalf("write replacement log: %v", err)
	}

	if _, err := h.HarnessRecordStop(); err == nil {
		t.Fatal("HarnessRecordStop sliced a rotated-and-regrown event log")
	}
}

// TestHarnessResetClearsHarnessState: reset is the per-test isolation
// primitive — scenario rules, the in-flight recording, and generated
// seed workspaces must not survive it.
func TestHarnessResetClearsHarnessState(t *testing.T) {
	h, _ := newHarnessTestHost(t)

	if _, err := h.HarnessSetScenario(HarnessScenarioSpec{Name: "streaming-text"}); err != nil {
		t.Fatalf("HarnessSetScenario: %v", err)
	}
	if _, err := h.HarnessRecordStart("doomed", "thread-r"); err != nil {
		t.Fatalf("HarnessRecordStart: %v", err)
	}
	workspaces := filepath.Join(h.config.DataRoot, "workspaces", "proj")
	if err := os.MkdirAll(workspaces, 0o755); err != nil {
		t.Fatalf("mkdir workspaces: %v", err)
	}

	if err := h.HarnessReset(); err != nil {
		t.Fatalf("HarnessReset: %v", err)
	}

	rules, err := h.HarnessListScenarios()
	if err != nil || len(rules.Rules) != 0 {
		t.Fatalf("scenario rules survived reset: %+v, %v", rules.Rules, err)
	}
	if _, err := os.Stat(filepath.Join(h.config.DataDir, "bundles", "doomed")); !os.IsNotExist(err) {
		t.Fatalf("in-flight recording dir survived reset (stat err %v)", err)
	}
	if _, err := os.Stat(filepath.Join(h.config.DataRoot, "workspaces")); !os.IsNotExist(err) {
		t.Fatalf("generated workspaces survived reset (stat err %v)", err)
	}
	// The aborted recording released its slot: a new one can start.
	if _, err := h.HarnessRecordStart("fresh", "thread-r"); err != nil {
		t.Fatalf("HarnessRecordStart after reset: %v", err)
	}
}

// TestHarnessReplayBundleRefusesActiveReplay: the active-replay check
// must run BEFORE sessions are stopped and the DB is restored —
// Replayer.Start would refuse anyway, but by then the restore has
// already destroyed post-snapshot state while the old replay keeps
// emitting over it.
func TestHarnessReplayBundleRefusesActiveReplay(t *testing.T) {
	h, host := newHarnessTestHost(t)
	const threadID = "thread-guard"

	if _, err := h.HarnessRecordStart("guard", threadID); err != nil {
		t.Fatalf("HarnessRecordStart: %v", err)
	}
	logHarnessEvent(t, host, time.Now(), threadID, "provider:item_event", map[string]any{"n": 1})
	if _, err := h.HarnessRecordStop(); err != nil {
		t.Fatalf("HarnessRecordStop: %v", err)
	}

	// Written after the snapshot: a restore would erase it.
	seedHarnessThread(t, host.store, "post-snapshot-thread")

	// Park a paused replay, then try to restore the bundle over it.
	events := filepath.Join(h.config.DataDir, "bundles", "guard", "events.jsonl")
	if _, err := h.HarnessReplayStart(events, harness.ReplayOptions{StartPaused: true}); err != nil {
		t.Fatalf("HarnessReplayStart: %v", err)
	}
	defer h.HarnessReplayStop()

	if _, err := h.HarnessReplayBundle("guard", harness.ReplayOptions{}); err == nil {
		t.Fatal("HarnessReplayBundle succeeded while a replay was active")
	}
	if _, err := host.store.GetThread("post-snapshot-thread"); err != nil {
		t.Fatalf("post-snapshot thread gone — the DB was restored despite the active-replay refusal: %v", err)
	}
}

// TestHarnessReplayBundleRefusesCorruptEvents: a damaged events file
// must fail the RPC before sessions are stopped and the DB is replaced —
// an error return paired with destroyed live state is the worst of both.
func TestHarnessReplayBundleRefusesCorruptEvents(t *testing.T) {
	h, host := newHarnessTestHost(t)
	const threadID = "thread-corrupt"

	if _, err := h.HarnessRecordStart("corrupt", threadID); err != nil {
		t.Fatalf("HarnessRecordStart: %v", err)
	}
	logHarnessEvent(t, host, time.Now(), threadID, "provider:item_event", map[string]any{"n": 1})
	if _, err := h.HarnessRecordStop(); err != nil {
		t.Fatalf("HarnessRecordStop: %v", err)
	}
	events := filepath.Join(h.config.DataDir, "bundles", "corrupt", "events.jsonl")
	if err := os.WriteFile(events, []byte("{not json}\n"), 0o600); err != nil {
		t.Fatalf("corrupt events file: %v", err)
	}

	// Written after the snapshot: a restore would erase it.
	seedHarnessThread(t, host.store, "survives-corrupt-bundle")

	if _, err := h.HarnessReplayBundle("corrupt", harness.ReplayOptions{}); err == nil {
		t.Fatal("HarnessReplayBundle accepted a corrupt events file")
	}
	if _, err := host.store.GetThread("survives-corrupt-bundle"); err != nil {
		t.Fatalf("live state destroyed despite the corrupt-events refusal: %v", err)
	}
}

func TestHarnessReplayBundleMissing(t *testing.T) {
	h, _ := newHarnessTestHost(t)
	if _, err := h.HarnessReplayBundle("no-such-bundle", harness.ReplayOptions{}); err == nil {
		t.Fatal("HarnessReplayBundle succeeded for a missing bundle")
	}
}

func TestHarnessEmitValidation(t *testing.T) {
	h, host := newHarnessTestHost(t)
	var got string
	host.emit = func(channel eventchan.Channel, data any) { got = channel.String() }

	if err := h.HarnessEmit("", json.RawMessage(`{}`)); err == nil {
		t.Fatal("HarnessEmit accepted an empty channel")
	}
	if err := h.HarnessEmit("c", nil); err == nil {
		t.Fatal("HarnessEmit accepted an empty payload")
	}
	if err := h.HarnessEmit("c", json.RawMessage(`{bad`)); err == nil {
		t.Fatal("HarnessEmit accepted invalid JSON")
	}
	if err := h.HarnessEmit("provider:item_event", json.RawMessage(`{"ok":true}`)); err != nil {
		t.Fatalf("HarnessEmit: %v", err)
	}
	if got != "provider:item_event" {
		t.Fatalf("emitted channel = %q", got)
	}
}

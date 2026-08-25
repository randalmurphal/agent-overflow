// app_harness_replay.go — wire-level replay RPCs: re-emit recorded
// event streams (with original timing) onto the live bus, and
// capture/replay bundles that pair the event stream with a DB snapshot
// so lazy loads resolve exactly as the original session.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"agent-overflow/internal/atomicfile"
	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/harness"
)

// recordDrainTimeout bounds the wait for the event-log queue to flush
// before a capture point. The queue drains in microseconds normally;
// hitting this means the log writer is wedged and the recording would
// silently miss events — fail instead.
const recordDrainTimeout = 5 * time.Second

// harnessRecording tracks the one in-flight recording.
type harnessRecording struct {
	Name      string `json:"name"`
	ThreadID  string `json:"threadId"`
	StartedAt int64  `json:"startedAt"`
	// offset is the event-log byte size at start; the bundle's events
	// are everything appended after it.
	offset int64
	dir    string
	// logAtStart / backupAtStart pin the identity of the event-log file
	// (and its newest rotation backup) at the offset capture, so
	// RecordStop can detect a rotation instead of silently slicing a
	// replacement file. Nil when the file didn't exist yet.
	logAtStart    os.FileInfo
	backupAtStart os.FileInfo
	// lostAtStart snapshots the event-log loss counter (queue drops +
	// write failures); any increase across the window means the bundle
	// would be missing events.
	lostAtStart int64
}

// HarnessBundleMeta is the bundle's meta.json — enough for a later
// session (or another agent) to know what it's replaying.
type HarnessBundleMeta struct {
	Name       string `json:"name"`
	ThreadID   string `json:"threadId"`
	CreatedAt  int64  `json:"createdAt"`
	EventCount int    `json:"eventCount"`
	// SnapshotAt is "start": the DB snapshot was taken at RecordStart,
	// so replaying the events over the restored snapshot reproduces the
	// original session exactly.
	SnapshotAt string `json:"snapshotAt"`
}

// replayer lazily constructs the engine wired to the live bus.
func (h *Harness) replayerEngine() *harness.Replayer {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.replayer == nil {
		h.replayer = harness.NewReplayer(
			// Deliberate escape hatch: a recorded event log names its own
			// channels, so they are unregistrable by construction and land
			// on the fail-closed loopback-only default. Harness-only.
			func(kind string, data json.RawMessage) { h.app.emit(eventchan.Channel(kind), data) },
			func(st harness.ReplayStatus) { h.app.emit(eventchan.HarnessReplay, st) },
		)
	}
	return h.replayer
}

// HarnessReplayStart replays a raw event-log file (the NDJSON shape
// internal/observability/replay writes) without touching the DB. Use
// HarnessReplayBundle for snapshot-paired fidelity.
func (h *Harness) HarnessReplayStart(path string, opts harness.ReplayOptions) (harness.ReplayStatus, error) {
	return h.replayerEngine().Start(path, opts)
}

// HarnessReplayPause suspends the active replay.
func (h *Harness) HarnessReplayPause() harness.ReplayStatus { return h.replayerEngine().Pause() }

// HarnessReplayResume continues a paused replay.
func (h *Harness) HarnessReplayResume() harness.ReplayStatus { return h.replayerEngine().Resume() }

// HarnessReplayStep releases exactly one event of a paused replay.
func (h *Harness) HarnessReplayStep() (harness.ReplayStatus, error) {
	return h.replayerEngine().Step()
}

// HarnessReplayStop aborts the active replay.
func (h *Harness) HarnessReplayStop() harness.ReplayStatus { return h.replayerEngine().Stop() }

// HarnessReplayStatus reports replay state without side effects.
func (h *Harness) HarnessReplayStatus() harness.ReplayStatus { return h.replayerEngine().Status() }

// HarnessRecordStart begins capturing a replay bundle for a thread:
// snapshots the DB now and marks the event-log offset, so RecordStop
// packages exactly the events that follow. One recording at a time.
//
// The thread must be idle: with a turn in flight, an event could land
// in both the snapshot (its DB write) and the event slice (its log
// append) — or in neither — depending on where it straddles the two
// capture points. Refusing an active turn makes the boundary exact.
func (h *Harness) HarnessRecordStart(name, threadID string) (harnessRecording, error) {
	if err := validateBundleName(name); err != nil {
		return harnessRecording{}, err
	}
	if threadID == "" {
		return harnessRecording{}, fmt.Errorf("threadId must be non-empty")
	}
	if h.app.replay == nil || !h.app.replay.Enabled() {
		return harnessRecording{}, fmt.Errorf("event log is disabled; harness boots enable it — was the setting changed?")
	}
	if turn, ok, err := h.app.store.GetActiveTurn(threadID); err != nil {
		return harnessRecording{}, fmt.Errorf("check for active turn: %w", err)
	} else if ok {
		return harnessRecording{}, fmt.Errorf("thread %s has turn %s in flight; start the recording while the thread is idle so the snapshot/event boundary is exact", threadID, turn.TurnID)
	}

	// Reserve the recording slot before any I/O — concurrent RPCs must
	// not both pass the nil check and race their snapshots.
	placeholder := &harnessRecording{Name: name, ThreadID: threadID}
	h.mu.Lock()
	if h.recording != nil {
		rec := *h.recording
		h.mu.Unlock()
		return rec, fmt.Errorf("recording %q already active; stop it first", rec.Name)
	}
	h.recording = placeholder
	h.mu.Unlock()
	release := func() {
		h.mu.Lock()
		if h.recording == placeholder {
			h.recording = nil
		}
		h.mu.Unlock()
	}

	dir := filepath.Join(h.paths.DataDir, "bundles", name)
	if _, err := os.Stat(dir); err == nil {
		release()
		return harnessRecording{}, fmt.Errorf("bundle %q already exists at %s", name, dir)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		release()
		return harnessRecording{}, fmt.Errorf("create bundle dir: %w", err)
	}
	// From here on, failure must remove the half-built dir so the
	// bundle name isn't blocked by an unreadable husk.
	fail := func(err error) (harnessRecording, error) {
		if rmErr := os.RemoveAll(dir); rmErr != nil {
			err = fmt.Errorf("%w (and removing the incomplete bundle dir failed: %v)", err, rmErr)
		}
		release()
		return harnessRecording{}, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), recordDrainTimeout)
	defer cancel()
	if err := h.app.replay.WaitForDrain(ctx); err != nil {
		return fail(fmt.Errorf("event log did not drain before capture: %w", err))
	}
	offset := int64(0)
	var logAtStart os.FileInfo
	if info, err := os.Stat(h.eventLogPath(threadID)); err == nil {
		offset = info.Size()
		logAtStart = info
	}
	var backupAtStart os.FileInfo
	if info, err := os.Stat(h.eventLogPath(threadID) + ".1"); err == nil {
		backupAtStart = info
	}

	if err := h.app.store.SnapshotTo(filepath.Join(dir, "db.snapshot")); err != nil {
		return fail(err)
	}

	rec := harnessRecording{
		Name:          name,
		ThreadID:      threadID,
		StartedAt:     time.Now().UnixMilli(),
		offset:        offset,
		dir:           dir,
		logAtStart:    logAtStart,
		backupAtStart: backupAtStart,
		lostAtStart:   h.app.replay.LostCount(),
	}
	h.mu.Lock()
	if h.recording != placeholder {
		// A concurrent HarnessReset aborted this start.
		h.mu.Unlock()
		return fail(fmt.Errorf("recording %q was aborted while starting", name))
	}
	h.recording = &rec
	h.mu.Unlock()
	return rec, nil
}

// HarnessRecordStop finalises the active recording into its bundle and
// returns the bundle metadata. Failure discards the recording — the
// half-built bundle dir is removed so the name is reusable, and the
// error says what was lost. (Discard-on-failure beats a retryable slot:
// every failure here means the window is already unreliable.)
func (h *Harness) HarnessRecordStop() (HarnessBundleMeta, error) {
	h.mu.Lock()
	rec := h.recording
	h.recording = nil
	h.mu.Unlock()
	if rec == nil {
		return HarnessBundleMeta{}, fmt.Errorf("no active recording")
	}
	fail := func(err error) (HarnessBundleMeta, error) {
		if rmErr := os.RemoveAll(rec.dir); rmErr != nil {
			err = fmt.Errorf("%w (and removing the incomplete bundle dir failed: %v)", err, rmErr)
		}
		return HarnessBundleMeta{}, fmt.Errorf("recording %q discarded: %w", rec.Name, err)
	}

	if !h.app.replay.Enabled() {
		return fail(fmt.Errorf("event log was disabled during the recording window"))
	}
	ctx, cancel := context.WithTimeout(context.Background(), recordDrainTimeout)
	defer cancel()
	if err := h.app.replay.WaitForDrain(ctx); err != nil {
		return fail(fmt.Errorf("event log did not drain before capture: %w", err))
	}
	if lost := h.app.replay.LostCount() - rec.lostAtStart; lost > 0 {
		return fail(fmt.Errorf("%d event(s) were dropped or failed to write during the window; the bundle would be incomplete", lost))
	}

	events, err := h.copyEventSlice(rec)
	if err != nil {
		return fail(err)
	}

	meta := HarnessBundleMeta{
		Name:       rec.Name,
		ThreadID:   rec.ThreadID,
		CreatedAt:  time.Now().UnixMilli(),
		EventCount: events,
		SnapshotAt: "start",
	}
	if err := atomicfile.WriteJSON(filepath.Join(rec.dir, "meta.json"), meta); err != nil {
		return fail(fmt.Errorf("write bundle meta: %w", err))
	}
	return meta, nil
}

// copyEventSlice copies the thread's event log from the recorded offset
// into the bundle, returning the event (line) count. Rotation during
// the window is detected by file identity, not size — a replacement
// file can grow past the start offset and would otherwise be sliced as
// if it were the original.
func (h *Harness) copyEventSlice(rec *harnessRecording) (int, error) {
	src, err := os.Open(h.eventLogPath(rec.ThreadID))
	if err != nil {
		return 0, fmt.Errorf("open event log (did the thread emit anything?): %w", err)
	}
	defer src.Close()

	info, err := src.Stat()
	if err != nil {
		return 0, fmt.Errorf("stat event log: %w", err)
	}
	if err := rec.checkLogIdentity(info, h.eventLogPath(rec.ThreadID)+".1"); err != nil {
		return 0, err
	}
	if info.Size() < rec.offset {
		return 0, fmt.Errorf("event log rotated during the recording window (size %d < start offset %d); capture a shorter window", info.Size(), rec.offset)
	}
	if _, err := src.Seek(rec.offset, io.SeekStart); err != nil {
		return 0, fmt.Errorf("seek event log: %w", err)
	}
	data, err := io.ReadAll(src)
	if err != nil {
		return 0, fmt.Errorf("read event log slice: %w", err)
	}
	if len(data) == 0 {
		return 0, fmt.Errorf("no events were recorded between start and stop")
	}
	if err := os.WriteFile(filepath.Join(rec.dir, "events.jsonl"), data, 0o600); err != nil {
		return 0, fmt.Errorf("write bundle events: %w", err)
	}
	count := 0
	for _, b := range data {
		if b == '\n' {
			count++
		}
	}
	return count, nil
}

// HarnessReplayBundle restores a bundle's DB snapshot and starts
// replaying its event stream. Every provider session is stopped first —
// restoring the DB under a live session would desynchronise both.
// The caller should reload the page after this returns so the frontend
// rebuilds from the restored DB before the events land (StartPaused +
// reload + Resume is the deterministic sequence; the e2e helper does
// exactly that).
func (h *Harness) HarnessReplayBundle(name string, opts harness.ReplayOptions) (harness.ReplayStatus, error) {
	dir := name
	if !filepath.IsAbs(name) {
		if err := validateBundleName(name); err != nil {
			return harness.ReplayStatus{}, err
		}
		dir = filepath.Join(h.paths.DataDir, "bundles", name)
	}
	var meta HarnessBundleMeta
	if found, err := atomicfile.ReadJSON(filepath.Join(dir, "meta.json"), &meta); err != nil {
		return harness.ReplayStatus{}, fmt.Errorf("read bundle meta: %w", err)
	} else if !found {
		return harness.ReplayStatus{}, fmt.Errorf("bundle %q has no meta.json (incomplete recording?)", name)
	}
	// Refuse before the destructive work below. Replayer.Start enforces
	// one-replay-at-a-time and validates the recording anyway, but by
	// that point sessions are dead and the DB has already been replaced —
	// the RPC would report failure after destroying live state.
	if st := h.replayerEngine().Status(); st.State == "running" || st.State == "paused" {
		return st, fmt.Errorf("harness: replay already active (%s at %d/%d); stop it before restoring a bundle", st.State, st.Position, st.Total)
	}
	eventsPath := filepath.Join(dir, "events.jsonl")
	if err := harness.ValidateRecording(eventsPath, opts.ThreadFilter); err != nil {
		return harness.ReplayStatus{}, fmt.Errorf("bundle %q events unusable: %w", name, err)
	}

	threads, err := h.app.store.ListThreads()
	if err != nil {
		return harness.ReplayStatus{}, fmt.Errorf("list threads: %w", err)
	}
	for _, t := range threads {
		if err := h.app.StopSession(t.ID); err != nil {
			return harness.ReplayStatus{}, fmt.Errorf("stop session %s before restore: %w", t.ID, err)
		}
	}
	identity, err := h.app.store.RestoreFrom(filepath.Join(dir, "db.snapshot"))
	if err != nil {
		return harness.ReplayStatus{}, err
	}
	// Re-publish immediately: the bootstrap manifest and every sync
	// response serve from this cache, and a client that keeps seeing the
	// pre-restore generation never drops the replica the restore just
	// invalidated (docs/specs/thread-replica-sync.md §3.3).
	h.app.storeIdentity.Store(&identity)
	return h.replayerEngine().Start(eventsPath, opts)
}

// HarnessListBundles enumerates recorded bundles.
func (h *Harness) HarnessListBundles() ([]HarnessBundleMeta, error) {
	root := filepath.Join(h.paths.DataDir, "bundles")
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return []HarnessBundleMeta{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list bundles: %w", err)
	}
	out := make([]HarnessBundleMeta, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		var meta HarnessBundleMeta
		found, err := atomicfile.ReadJSON(filepath.Join(root, e.Name(), "meta.json"), &meta)
		if err != nil {
			// Corrupt/unreadable metadata is a real failure, not an
			// in-progress recording — silently hiding the bundle would
			// read as "it vanished".
			return nil, fmt.Errorf("bundle %q has unreadable metadata: %w", e.Name(), err)
		}
		if !found {
			continue // recording still in progress; not listable yet
		}
		out = append(out, meta)
	}
	return out, nil
}

// checkLogIdentity errors when the event log rotated during the
// recording window. Two signals: the current file is no longer the one
// whose offset was captured (identity change), or a new .1 backup
// appeared/changed since start (every rotation renames into .1 —
// this also covers the log being created *and* rotated entirely
// within the window, where the current file's identity was never
// pinned).
func (rec *harnessRecording) checkLogIdentity(current os.FileInfo, backupPath string) error {
	rotated := fmt.Errorf("event log rotated during the recording window; the bundle would be missing its earlier events — capture a shorter window (or raise the event-log MaxBytes)")
	if rec.logAtStart != nil && !os.SameFile(rec.logAtStart, current) {
		return rotated
	}
	if backup, err := os.Stat(backupPath); err == nil {
		if rec.backupAtStart == nil || !os.SameFile(rec.backupAtStart, backup) {
			return rotated
		}
	}
	return nil
}

func (h *Harness) eventLogPath(threadID string) string {
	return filepath.Join(h.paths.DataDir, "replay", threadID+".jsonl")
}

func validateBundleName(name string) error {
	if name == "" || strings.ContainsAny(name, `/\`) || name == "." || name == ".." {
		return fmt.Errorf("bundle name %q must be a plain directory name", name)
	}
	return nil
}

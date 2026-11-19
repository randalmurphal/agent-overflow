//go:build !windows

package app

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"agent-overflow/internal/orphanreaper"
	"agent-overflow/internal/store"
)

// A watch's script ends with the backend: its process group, with what the
// script started in the background, is recorded for the orphan reaper while
// it runs and released once the run has ended it.

// awaitGroupReleased waits until the registry is empty and the group runs
// nothing.
func awaitGroupReleased(t *testing.T, registry *orphanreaper.Registry, pgid int) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		records, err := registry.Load()
		if err != nil {
			t.Fatal(err)
		}
		if len(records) == 0 && !groupRunning(pgid) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("after the run ended the registry holds %+v, group running %v", records, groupRunning(pgid))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// requireWatchRecord checks that the record is the running group of watch.
func requireWatchRecord(t *testing.T, record orphanreaper.Record, watch store.AgentWake) {
	t.Helper()
	if record.Owner != "agent-wake:"+watch.ID || record.PGID != record.PID || !groupRunning(record.PGID) {
		t.Fatalf("record %+v, want the running group of watch %s", record, watch.ID)
	}
}

// The command leaves a background child in the group, so a release that
// came from the leader's exit alone would leave it running.
const watchWithBackgroundChild = "sleep 60 & echo started; wait"

func TestAnIntervalWatchScriptIsWatchedAndReleased(t *testing.T) {
	t.Parallel()
	a, _ := newAppForFlushQueueRPC(t)
	registry := orphanreaper.NewRegistry(filepath.Join(t.TempDir(), "orphan-registry.json"))
	a.orphanRegistry = registry
	thread := noticeTestThread(t, a, "watch-orphan-interval")
	testAssistant(t, a, thread.ID)
	watch := registerTestWake(t, a, store.AgentWake{
		ThreadID: thread.ID, Kind: store.AgentWakeWatch, Label: "inbox", Command: watchWithBackgroundChild,
		IntervalSeconds: 3600, NextAt: time.Now().Add(time.Hour).UnixMilli(),
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	run := &agentWakeRun{threadID: thread.ID, cancel: cancel, done: make(chan struct{})}
	result := make(chan agentWatchResult, 1)
	go func() { result <- a.runAgentWatchScript(ctx, run, watch) }()

	record := waitOrphanRecord(t, registry)
	requireWatchRecord(t, record, watch)
	cancel()
	select {
	case <-result:
	case <-time.After(15 * time.Second):
		t.Fatal("the cancelled watch never returned")
	}
	awaitGroupReleased(t, registry, record.PGID)
}

func TestALongRunningWatchProcessIsWatchedAndReleased(t *testing.T) {
	t.Parallel()
	a, _ := newAppForFlushQueueRPC(t)
	registry := orphanreaper.NewRegistry(filepath.Join(t.TempDir(), "orphan-registry.json"))
	a.orphanRegistry = registry
	thread := noticeTestThread(t, a, "watch-orphan-process")
	testAssistant(t, a, thread.ID)
	t.Cleanup(func() { a.stopAgentWakeRunsForThread(thread.ID) })
	watch := registerTestWake(t, a, store.AgentWake{
		ThreadID: thread.ID, Kind: store.AgentWakeWatch, Label: "tail", Command: watchWithBackgroundChild, LongRunning: true,
	})

	record := waitOrphanRecord(t, registry)
	requireWatchRecord(t, record, watch)
	a.stopAgentWakeRunsForThread(thread.ID)
	awaitGroupReleased(t, registry, record.PGID)
}

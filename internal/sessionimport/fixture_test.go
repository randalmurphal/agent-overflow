package sessionimport

import (
	"path/filepath"
	"testing"
	"time"

	"agent-overflow/internal/importir"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// fixture_test.go — the shared harness for this package's tests.
//
// Every store here is a fresh file under t.TempDir(); nothing in this
// package reads a provider home or spawns a provider binary, and nothing
// may start doing so (root AGENTS.md §Permanent invariants).

const (
	testProjectID = "project-sessionimport"
	testThreadID  = "thread-sessionimport"
)

// baseMillis is the fixture clock: a fixed wall time in the past, so a
// row that was restamped with now() is off by years rather than
// milliseconds.
const baseMillis int64 = 1_700_000_000_000

func at(offset int64) time.Time {
	return time.UnixMilli(baseMillis + offset)
}

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.New(filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// seedThread creates the project + thread rows an import writes into and
// returns the thread as NewWriter takes it.
func seedThread(t *testing.T, st *store.Store, threadID, providerName, workspace string) store.Thread {
	t.Helper()
	if _, err := st.GetProject(testProjectID); err != nil {
		if err := st.CreateProject(store.Project{
			ID:        testProjectID,
			Path:      workspace,
			Name:      "Session Import Tests",
			CreatedAt: baseMillis,
			UpdatedAt: baseMillis,
		}); err != nil {
			t.Fatalf("create project: %v", err)
		}
	}
	thread := store.Thread{
		ID:            threadID,
		ProjectID:     testProjectID,
		Title:         "Imported",
		Provider:      providerName,
		WorkspacePath: workspace,
		CreatedAt:     baseMillis,
		UpdatedAt:     baseMillis,
	}
	if err := st.CreateThread(thread); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	return thread
}

// importEvents lifts wire events into IR events, stamping the source
// coordinate every imported row carries provenance from. Claude-shaped:
// one transcript uuid per line.
func importEvents(events []provider.ProviderEvent) []importir.Event {
	out := make([]importir.Event, 0, len(events))
	for i, evt := range events {
		out = append(out, importir.Event{
			ProviderEvent: evt,
			SourceUUID:    sourceUUID(i),
		})
	}
	return out
}

func sourceUUID(i int) string {
	return "00000000-0000-4000-8000-" + pad12(i)
}

func pad12(i int) string {
	const digits = "0123456789abcdef"
	out := []byte("000000000000")
	for pos := len(out) - 1; pos >= 0 && i > 0; pos-- {
		out[pos] = digits[i%16]
		i /= 16
	}
	return string(out)
}

// buildAndApply runs the writer end to end into st and fails the test on
// any error or warning the fixture did not expect.
func buildAndApply(t *testing.T, st *store.Store, thread store.Thread, events []importir.Event) []importir.Warning {
	t.Helper()
	batch, warnings, err := NewWriter(st, thread).Build(events)
	if err != nil {
		t.Fatalf("build import batch: %v", err)
	}
	if err := st.ApplyImportBatch(thread.ID, batch); err != nil {
		t.Fatalf("apply import batch: %v", err)
	}
	return warnings
}

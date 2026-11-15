package app

import (
	"database/sql"
	"errors"
	"testing"

	"agent-overflow/internal/store"
)

// The composer decides to delete an emptied draft thread once its empty
// state is saved, and the delete reaches the backend a round trip later.
// Whatever the thread gained in between, from this screen or another, is
// what the delete decides against: it keeps the thread and says so.
func TestEmptyDraftCleanupKeepsWhatArrivedAfterTheDecision(t *testing.T) {
	t.Parallel()
	for name, gain := range map[string]func(*testing.T, *App){
		"text": func(t *testing.T, app *App) {
			if err := app.SaveDraft(t.Context(), "thr-a", "typed again", nil, nil, nil); err != nil {
				t.Fatal(err)
			}
		},
		"attachment": func(t *testing.T, app *App) {
			if err := app.SaveDraft(t.Context(), "thr-a", "", []string{"att-1"}, nil, nil); err != nil {
				t.Fatal(err)
			}
		},
		"terminal chip": func(t *testing.T, app *App) {
			chip := TerminalChip{ID: "chip-1", Label: "shell", Content: "$ ls", CreatedAt: 1}
			if err := app.SaveDraft(t.Context(), "thr-a", "", nil, []TerminalChip{chip}, nil); err != nil {
				t.Fatal(err)
			}
		},
		"plan link": func(t *testing.T, app *App) {
			plan := &SourceProposedPlan{ThreadID: "src", ItemID: "plan-1", PayloadID: "payload-1", Title: "Plan"}
			if err := app.SaveDraft(t.Context(), "thr-a", "", nil, nil, plan); err != nil {
				t.Fatal(err)
			}
		},
		"queued message": func(t *testing.T, app *App) {
			if err := app.store.InsertFlushQueueItem(store.FlushQueueItem{ID: "queued-1", ThreadID: "thr-a", Message: "queued", EnqueuedAt: 1}); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			app := newAttachmentTestApp(t)
			// The client's decision: the erased draft has been saved empty.
			if err := app.SaveDraft(t.Context(), "thr-a", "", nil, nil, nil); err != nil {
				t.Fatal(err)
			}
			gain(t, app)
			deleted, err := app.DeleteEmptyDraftThread("thr-a")
			if err != nil || deleted {
				t.Fatalf("DeleteEmptyDraftThread = %v, %v; want the thread kept", deleted, err)
			}
			if _, err := app.store.GetThread("thr-a"); err != nil {
				t.Fatalf("thread after refused cleanup: %v", err)
			}
		})
	}
}

// A save that reaches the backend after the cleanup deleted its thread is
// refused as not_found (sql.ErrNoRows at the transport), not as constraint
// text, and leaves no row behind.
func TestSaveDraftForADeletedEmptyDraftIsNotFound(t *testing.T) {
	t.Parallel()
	app := newAttachmentTestApp(t)
	if deleted, err := app.DeleteEmptyDraftThread("thr-a"); err != nil || !deleted {
		t.Fatalf("DeleteEmptyDraftThread = %v, %v", deleted, err)
	}
	err := app.SaveDraft(t.Context(), "thr-a", "typed during the delete", nil, nil, nil)
	if !errors.Is(err, store.ErrDraftThreadMissing) || !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("SaveDraft after delete = %v, want ErrDraftThreadMissing", err)
	}
	if _, ok, err := app.store.GetThreadDraft("thr-a"); err != nil || ok {
		t.Fatalf("draft row after refused save: ok=%v err=%v", ok, err)
	}
}

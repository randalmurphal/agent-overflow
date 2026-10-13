package threadapp

import (
	"database/sql"
	"errors"
	"slices"
	"testing"

	"agent-overflow/internal/store"
)

func TestDeleteThreadPreservesResourceOrder(t *testing.T) {
	service, database, _ := newServiceFixture(t)
	thread, err := service.Create(CreateOptions{ProjectID: "project"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	var calls []string
	ports := DeletePorts{
		CleanProviderBackground: func(thread store.Thread) error { calls = append(calls, "background"); return nil },
		StopSession:             func(id string) error { calls = append(calls, "session"); return nil },
		CancelWorktreeSetup:     func(id string) { calls = append(calls, "setup") },
		CloseTerminals:          func(id string) error { calls = append(calls, "terminal"); return nil },
		CloseBrowserPages:       func(id string) error { calls = append(calls, "browser"); return nil },
		ClearAutoReconnect:      func(id string) { calls = append(calls, "reconnect") },
		CleanupAttachments:      func(id string) error { calls = append(calls, "attachments"); return nil },
		CleanupReplayLog:        func(id string) error { calls = append(calls, "replay"); return nil },
		Forget:                  func(id string) { calls = append(calls, "forget") },
	}
	if err := service.DeleteThread(thread.ID, ports); err != nil {
		t.Fatalf("DeleteThread: %v", err)
	}
	want := []string{
		"background", "session", "setup", "terminal", "browser", "reconnect",
		"attachments", "replay", "terminal", "browser", "forget",
	}
	if !slices.Equal(calls, want) {
		t.Fatalf("cleanup order = %v, want %v", calls, want)
	}
	if _, err := database.GetThread(thread.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetThread error = %v, want sql.ErrNoRows", err)
	}
}

func TestDeleteThreadContinuesCleanupButPreservesRowOnFailure(t *testing.T) {
	service, database, _ := newServiceFixture(t)
	thread, err := service.Create(CreateOptions{ProjectID: "project"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	var calls []string
	cleanupErr := errors.New("disk busy")
	browserErr := errors.New("profile busy")
	err = service.DeleteThread(thread.ID, DeletePorts{
		StopSession:       func(string) error { calls = append(calls, "session"); return nil },
		CloseBrowserPages: func(string) error { calls = append(calls, "browser"); return browserErr },
		CleanupAttachments: func(string) error {
			calls = append(calls, "attachments")
			return cleanupErr
		},
		CleanupReplayLog: func(string) error { calls = append(calls, "replay"); return nil },
		Forget:           func(string) { calls = append(calls, "forget") },
	})
	if !errors.Is(err, cleanupErr) || !errors.Is(err, browserErr) {
		t.Fatalf("DeleteThread error = %v, want disk and browser errors", err)
	}
	if !slices.Equal(calls, []string{"session", "browser", "attachments", "replay"}) {
		t.Fatalf("cleanup calls = %v", calls)
	}
	if _, err := database.GetThread(thread.ID); err != nil {
		t.Fatalf("row deleted after failed cleanup: %v", err)
	}
}

// From its delete mark on a thread is gone to every read, so a delete that
// fails after the mark still reports the row deleted, for clients to drop
// it, and the next boot finishes it. One that fails before the mark keeps
// the row listed and reports nothing.
func TestDeleteThreadReportsTheRowOnceItsDeleteMarkCommits(t *testing.T) {
	service, database, _ := newServiceFixture(t)
	afterMark, err := service.Create(CreateOptions{ProjectID: "project"})
	if err != nil {
		t.Fatal(err)
	}
	service.deps.NewID = func() string { return "before-mark" }
	beforeMark, err := service.Create(CreateOptions{ProjectID: "project"})
	if err != nil {
		t.Fatal(err)
	}
	var deleted []string
	report := func(thread store.Thread, _ []string) { deleted = append(deleted, thread.ID) }

	cleanupErr := errors.New("disk busy")
	err = service.DeleteThread(afterMark.ID, DeletePorts{
		CleanupAttachments: func(string) error { return cleanupErr },
		Deleted:            report,
	})
	if !errors.Is(err, cleanupErr) {
		t.Fatalf("DeleteThread error = %v, want the cleanup failure", err)
	}
	if !slices.Equal(deleted, []string{afterMark.ID}) {
		t.Fatalf("deleted = %v after a failure past the mark, want the thread", deleted)
	}
	if pending, err := database.ListPendingThreadDeletes(); err != nil || !slices.Equal(pending, []string{afterMark.ID}) {
		t.Fatalf("pending deletes = %v, %v; want the thread for the next boot", pending, err)
	}

	deleted = nil
	remoteErr := errors.New("remote command admitted during cleanup could not be stopped")
	calls := 0
	err = service.DeleteThread(beforeMark.ID, DeletePorts{
		StopRemoteWork: func(string) error {
			calls++
			if calls == 2 {
				return remoteErr
			}
			return nil
		},
		Deleted: report,
	})
	if !errors.Is(err, remoteErr) {
		t.Fatalf("DeleteThread error = %v, want the refusal before the mark", err)
	}
	if len(deleted) != 0 {
		t.Fatalf("deleted = %v after a failure before the mark, want none", deleted)
	}
	if _, err := service.Get(beforeMark.ID); err != nil {
		t.Fatalf("Get after a failure before the mark: %v, want the thread listed", err)
	}
}

// The second close catches a page a companion action opened while the row
// was present. Its failure is reported, and the delete it follows stands.
func TestDeleteThreadClosesBrowserPagesAgainAfterTheRowDrops(t *testing.T) {
	service, database, _ := newServiceFixture(t)
	thread, err := service.Create(CreateOptions{ProjectID: "project"})
	if err != nil {
		t.Fatal(err)
	}
	var rowPresent []bool
	var forgotten, deleted bool
	closeErr := errors.New("profile busy")
	err = service.DeleteThread(thread.ID, DeletePorts{
		CloseBrowserPages: func(id string) error {
			_, err := database.GetThread(id)
			rowPresent = append(rowPresent, err == nil)
			if len(rowPresent) == 2 {
				return closeErr
			}
			return nil
		},
		Forget:  func(string) { forgotten = true },
		Deleted: func(store.Thread, []string) { deleted = true },
	})
	if !errors.Is(err, closeErr) {
		t.Fatalf("DeleteThread error = %v, want the second close's error", err)
	}
	if !slices.Equal(rowPresent, []bool{true, false}) {
		t.Fatalf("browser closes saw the row present = %v, want before and after the drop", rowPresent)
	}
	if !forgotten || !deleted {
		t.Fatalf("forgotten = %v, deleted = %v after the row dropped, want both", forgotten, deleted)
	}
	if _, err := database.GetThread(thread.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetThread error = %v, want sql.ErrNoRows", err)
	}
}

// The second close catches a terminal a client opened while the row was
// present. Its failure is reported, and the delete it follows stands.
func TestDeleteThreadClosesTerminalsAgainAfterTheRowDrops(t *testing.T) {
	service, database, _ := newServiceFixture(t)
	thread, err := service.Create(CreateOptions{ProjectID: "project"})
	if err != nil {
		t.Fatal(err)
	}
	var rowPresent []bool
	var forgotten, deleted bool
	closeErr := errors.New("pty busy")
	err = service.DeleteThread(thread.ID, DeletePorts{
		CloseTerminals: func(id string) error {
			_, err := database.GetThread(id)
			rowPresent = append(rowPresent, err == nil)
			if len(rowPresent) == 2 {
				return closeErr
			}
			return nil
		},
		Forget:  func(string) { forgotten = true },
		Deleted: func(store.Thread, []string) { deleted = true },
	})
	if !errors.Is(err, closeErr) {
		t.Fatalf("DeleteThread error = %v, want the second close's error", err)
	}
	if !slices.Equal(rowPresent, []bool{true, false}) {
		t.Fatalf("terminal closes saw the row present = %v, want before and after the drop", rowPresent)
	}
	if !forgotten || !deleted {
		t.Fatalf("forgotten = %v, deleted = %v after the row dropped, want both", forgotten, deleted)
	}
	if _, err := database.GetThread(thread.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetThread error = %v, want sql.ErrNoRows", err)
	}
}

func TestDeleteThreadStopsRemoteWorkAdmittedDuringCleanup(t *testing.T) {
	service, database, _ := newServiceFixture(t)
	thread, err := service.Create(CreateOptions{ProjectID: "project"})
	if err != nil {
		t.Fatal(err)
	}
	admitted := false
	busy := errors.New("remote command admitted during cleanup could not be stopped")
	err = service.DeleteThread(thread.ID, DeletePorts{
		StopRemoteWork: func(string) error {
			if admitted {
				return busy
			}
			return nil
		},
		StopSession: func(string) error { admitted = true; return nil },
		CleanupAttachments: func(string) error {
			t.Fatal("removed files belonging to newly admitted remote work")
			return nil
		},
	})
	if !errors.Is(err, busy) {
		t.Fatalf("deletion error: %v", err)
	}
	if _, err := database.GetThread(thread.ID); err != nil {
		t.Fatalf("deleted source of newly admitted remote work: %v", err)
	}
}

package threadapp

import (
	"database/sql"
	"errors"
	"slices"
	"testing"

	"agent-overflow/internal/store"
	"agent-overflow/internal/threadmode"
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
		CancelShellCommand:      func(id string) { calls = append(calls, "shell") },
		CloseTerminals:          func(id string) error { calls = append(calls, "terminal"); return nil },
		CloseBrowserPages:       func(id string) error { calls = append(calls, "browser"); return nil },
		ClearAutoReconnect:      func(id string) { calls = append(calls, "reconnect") },
		CleanupAttachments:      func(id string) error { calls = append(calls, "attachments"); return nil },
		CleanupReplayLog:        func(id string) error { calls = append(calls, "replay"); return nil },
		CleanupMemoryRecords:    func(id string) error { calls = append(calls, "records"); return nil },
		Forget:                  func(id string) { calls = append(calls, "forget") },
	}
	// Nothing of the thread stops before its claim commits.
	ports.CleanProviderBackground = func(thread store.Thread) error {
		if pending, err := database.ListPendingThreadDeletes(); err != nil || !slices.Equal(pending, []string{thread.ID}) {
			t.Errorf("pending deletes as the thread stops = %v, %v; want it claimed first", pending, err)
		}
		calls = append(calls, "background")
		return nil
	}
	if err := service.DeleteThread(thread.ID, ports); err != nil {
		t.Fatalf("DeleteThread: %v", err)
	}
	want := []string{
		"background", "session", "setup", "shell", "terminal", "browser", "reconnect",
		// The second stop is under the mutation lock a command registers with.
		"shell", "attachments", "replay", "records", "terminal", "browser", "forget",
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
	recordsErr := errors.New("records busy")
	err = service.DeleteThread(thread.ID, DeletePorts{
		StopSession:       func(string) error { calls = append(calls, "session"); return nil },
		CloseBrowserPages: func(string) error { calls = append(calls, "browser"); return browserErr },
		CleanupAttachments: func(string) error {
			calls = append(calls, "attachments")
			return cleanupErr
		},
		CleanupReplayLog: func(string) error { calls = append(calls, "replay"); return nil },
		CleanupMemoryRecords: func(string) error {
			calls = append(calls, "records")
			return recordsErr
		},
		Forget: func(string) { calls = append(calls, "forget") },
	})
	if !errors.Is(err, cleanupErr) || !errors.Is(err, browserErr) || !errors.Is(err, recordsErr) {
		t.Fatalf("DeleteThread error = %v, want disk, browser and records errors", err)
	}
	if !slices.Equal(calls, []string{"session", "browser", "attachments", "replay", "records"}) {
		t.Fatalf("cleanup calls = %v", calls)
	}
	if _, err := database.GetThread(thread.ID); err != nil {
		t.Fatalf("row deleted after failed cleanup: %v", err)
	}
}

// From its claim, the delete mark and the delete's first write, a thread is
// gone to every read, so a delete that fails after it still reports the row
// deleted, for clients to drop it, and the next boot finishes it. One
// refused before the claim keeps the row listed and reports nothing.
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
	homeErr := errors.New("this assistant was not deleted: its home is not its own folder")
	stopped := false
	err = service.DeleteThread(beforeMark.ID, DeletePorts{
		AssistantHome: func(string) (func() error, error) { return nil, homeErr },
		StopSession:   func(string) error { stopped = true; return nil },
		Deleted:       report,
	})
	if !errors.Is(err, homeErr) {
		t.Fatalf("DeleteThread error = %v, want the refusal before the mark", err)
	}
	if stopped {
		t.Fatal("a delete refused before its claim stopped the session")
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

// spawnWorker records a local spawn from owner that made worker, owned.
func spawnWorker(t *testing.T, database *store.Store, token, owner, worker string) {
	t.Helper()
	if _, err := database.AcceptLocalThreadRequest(store.ThreadRequest{
		Token: token, CallerThreadID: owner, Kind: store.ThreadRequestSpawn, State: store.ThreadRequestUnconfirmed,
	}, store.ThreadRequestReceipt{Token: token, OwnerDeviceID: "device", Kind: store.ThreadRequestSpawn, SourceThreadID: owner}); err != nil {
		t.Fatalf("accept %s: %v", token, err)
	}
	if err := database.NameLocalThreadRequestTarget(token, worker, true); err != nil {
		t.Fatalf("name %s: %v", token, err)
	}
}

// A worker's claim for its owner's delete is fenced by ownership: one that
// is no longer the owner's worker is left unclaimed. Once claimed it stays
// the owner's: Show in Sidebar and a group move are refused saying why. A
// released claim lists it again and lets it move.
func TestClaimDeleteIsFencedByOwnership(t *testing.T) {
	service, database, _ := newServiceFixture(t)
	create := func(id string) store.Thread {
		t.Helper()
		thread := store.Thread{
			ID: id, ProjectID: "project", Title: id, Provider: "claude", Model: "claude-opus-4-7",
			Mode: threadmode.ModeChat, WorkspacePath: "/repo", RuntimeMode: "approval-required", CreatedAt: 1, UpdatedAt: 1,
		}
		if err := database.CreateThread(thread); err != nil {
			t.Fatalf("CreateThread(%s): %v", id, err)
		}
		return thread
	}
	owner, placed, worker := create("owner"), create("placed"), create("worker")
	spawnWorker(t, database, "tok-placed", owner.ID, placed.ID)
	spawnWorker(t, database, "tok-worker", owner.ID, worker.ID)
	if _, err := database.SetThreadPlacement([]string{placed.ID}, store.PlacementStandalone); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ClaimDelete(placed.ID, owner.ID); !errors.Is(err, store.ErrWorkerNotOwned) {
		t.Fatalf("a placed worker's claim = %v, want ErrWorkerNotOwned", err)
	}
	if pending, err := database.ListPendingThreadDeletes(); err != nil || len(pending) != 0 {
		t.Fatalf("pending deletes = %v, %v; want the placed worker unclaimed", pending, err)
	}

	fresh, err := service.ClaimDelete(worker.ID, owner.ID)
	if err != nil || !fresh {
		t.Fatalf("the owner's worker = fresh %v, %v; want claimed", fresh, err)
	}
	if fresh, err := service.ClaimDelete(worker.ID, owner.ID); err != nil || fresh {
		t.Fatalf("claimed again = fresh %v, %v; want the claim kept, not fresh", fresh, err)
	}
	if _, err := database.SetThreadPlacement([]string{worker.ID}, store.PlacementStandalone); !errors.Is(err, store.ErrThreadDeleting) {
		t.Fatalf("Show in Sidebar on a claimed worker = %v, want ErrThreadDeleting", err)
	}
	group, err := database.CreateThreadGroup("project", "g")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.SetThreadGroup([]string{worker.ID}, group.ID); !errors.Is(err, store.ErrThreadDeleting) {
		t.Fatalf("a group move of a claimed worker = %v, want ErrThreadDeleting", err)
	}
	if row, err := database.GetThread(worker.ID); err != nil || row.Placement != store.PlacementOwned {
		t.Fatalf("the claimed worker = %+v, %v; want it still owned", row, err)
	}

	if err := service.ReleaseDelete(worker.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Get(worker.ID); err != nil {
		t.Fatalf("the released worker: %v, want it read again", err)
	}
	if _, err := database.SetThreadPlacement([]string{worker.ID}, store.PlacementStandalone); err != nil {
		t.Fatalf("Show in Sidebar after the claim was released: %v", err)
	}
}

package threadapp

import (
	"database/sql"
	"errors"
	"slices"
	"testing"

	"agent-overflow/internal/store"
)

func TestDeleteTreeRunsChildrenFirstAndPreservesResourceOrder(t *testing.T) {
	service, database, _ := newServiceFixture(t)
	parent, err := service.Create(CreateOptions{ProjectID: "project"})
	if err != nil {
		t.Fatalf("Create parent: %v", err)
	}
	service.deps.NewID = func() string { return "child" }
	child, err := service.Create(CreateOptions{ProjectID: "project"})
	if err != nil {
		t.Fatalf("Create child: %v", err)
	}
	child.ParentThreadID = parent.ID
	if err := database.UpdateThread(child); err != nil {
		t.Fatalf("UpdateThread child parent: %v", err)
	}

	var calls []string
	ports := DeletePorts{
		CleanProviderBackground: func(thread store.Thread) error { calls = append(calls, thread.ID+":background"); return nil },
		StopSession:             func(id string) error { calls = append(calls, id+":session"); return nil },
		CancelWorktreeSetup:     func(id string) { calls = append(calls, id+":setup") },
		CloseTerminals:          func(id string) error { calls = append(calls, id+":terminal"); return nil },
		CloseBrowserPages:       func(id string) error { calls = append(calls, id+":browser"); return nil },
		ClearSystemPrompt:       func(id string) { calls = append(calls, id+":prompt") },
		RemoveDiscussion:        func(thread store.Thread) { calls = append(calls, thread.ID+":discussion") },
		ClearAutoReconnect:      func(id string) { calls = append(calls, id+":reconnect") },
		CleanupAttachments:      func(id string) error { calls = append(calls, id+":attachments"); return nil },
		CleanupReplayLog:        func(id string) error { calls = append(calls, id+":replay"); return nil },
		Forget:                  func(id string) { calls = append(calls, id+":forget") },
	}
	if err := service.DeleteTree(parent.ID, false, ports); err != nil {
		t.Fatalf("DeleteTree: %v", err)
	}
	wantChild := []string{
		"child:background", "child:session", "child:setup", "child:terminal",
		"child:browser", "child:prompt", "child:discussion", "child:reconnect",
		"child:attachments", "child:replay", "child:browser", "child:forget",
	}
	if !slices.Equal(calls[:len(wantChild)], wantChild) {
		t.Fatalf("child cleanup order = %v, want %v", calls[:len(wantChild)], wantChild)
	}
	if last := calls[len(calls)-1]; last != parent.ID+":forget" {
		t.Fatalf("parent cleanup ended with %q, want its forget after the row drop", last)
	}
	if _, err := database.GetThread(parent.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetThread(parent) error = %v, want sql.ErrNoRows", err)
	}
}

func TestDeleteTreeContinuesCleanupButPreservesRowOnFailure(t *testing.T) {
	service, database, _ := newServiceFixture(t)
	thread, err := service.Create(CreateOptions{ProjectID: "project"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	var calls []string
	cleanupErr := errors.New("disk busy")
	browserErr := errors.New("profile busy")
	err = service.DeleteTree(thread.ID, false, DeletePorts{
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
		t.Fatalf("DeleteTree error = %v, want disk and browser errors", err)
	}
	if !slices.Equal(calls, []string{"session", "browser", "attachments", "replay"}) {
		t.Fatalf("cleanup calls = %v", calls)
	}
	if _, err := database.GetThread(thread.ID); err != nil {
		t.Fatalf("row deleted after failed cleanup: %v", err)
	}
}

// The second close catches a page a companion action opened while the row
// was present. Its failure is reported, and the delete it follows stands.
func TestDeleteTreeClosesBrowserPagesAgainAfterTheRowDrops(t *testing.T) {
	service, database, _ := newServiceFixture(t)
	thread, err := service.Create(CreateOptions{ProjectID: "project"})
	if err != nil {
		t.Fatal(err)
	}
	var rowPresent []bool
	var forgotten, deleted bool
	closeErr := errors.New("profile busy")
	err = service.DeleteTree(thread.ID, false, DeletePorts{
		CloseBrowserPages: func(id string) error {
			_, err := database.GetThread(id)
			rowPresent = append(rowPresent, err == nil)
			if len(rowPresent) == 2 {
				return closeErr
			}
			return nil
		},
		Forget:  func(string) { forgotten = true },
		Deleted: func(store.Thread) { deleted = true },
	})
	if !errors.Is(err, closeErr) {
		t.Fatalf("DeleteTree error = %v, want the second close's error", err)
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

func TestDeleteTreeStopsRemoteWorkAdmittedDuringCleanup(t *testing.T) {
	service, database, _ := newServiceFixture(t)
	thread, err := service.Create(CreateOptions{ProjectID: "project"})
	if err != nil {
		t.Fatal(err)
	}
	admitted := false
	busy := errors.New("remote command admitted during cleanup could not be stopped")
	err = service.DeleteTree(thread.ID, false, DeletePorts{
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

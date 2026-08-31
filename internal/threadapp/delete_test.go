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
		ClearSystemPrompt:       func(id string) { calls = append(calls, id+":prompt") },
		RemoveDiscussion:        func(thread store.Thread) { calls = append(calls, thread.ID+":discussion") },
		ClearAutoReconnect:      func(id string) { calls = append(calls, id+":reconnect") },
		CleanupAttachments:      func(id string) error { calls = append(calls, id+":attachments"); return nil },
		CleanupReplayLog:        func(id string) error { calls = append(calls, id+":replay"); return nil },
	}
	if err := service.DeleteTree(parent.ID, false, ports); err != nil {
		t.Fatalf("DeleteTree: %v", err)
	}
	wantChild := []string{
		"child:background", "child:session", "child:setup", "child:terminal",
		"child:prompt", "child:discussion", "child:reconnect", "child:attachments",
		"child:replay",
	}
	if !slices.Equal(calls[:len(wantChild)], wantChild) {
		t.Fatalf("child cleanup order = %v, want %v", calls[:len(wantChild)], wantChild)
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
	err = service.DeleteTree(thread.ID, false, DeletePorts{
		StopSession: func(string) error { calls = append(calls, "session"); return nil },
		CleanupAttachments: func(string) error {
			calls = append(calls, "attachments")
			return cleanupErr
		},
		CleanupReplayLog: func(string) error { calls = append(calls, "replay"); return nil },
	})
	if !errors.Is(err, cleanupErr) {
		t.Fatalf("DeleteTree error = %v, want disk error", err)
	}
	if !slices.Equal(calls, []string{"session", "attachments", "replay"}) {
		t.Fatalf("cleanup calls = %v", calls)
	}
	if _, err := database.GetThread(thread.ID); err != nil {
		t.Fatalf("row deleted after failed cleanup: %v", err)
	}
}

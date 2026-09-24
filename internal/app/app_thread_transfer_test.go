package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/attachment"
	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude/sessionfork"
	"agent-overflow/internal/providerdiscoveryapp"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
	"github.com/google/uuid"
)

func transferTestBackend(t *testing.T, readiness ...func(context.Context, string) error) *App {
	t.Helper()
	return transferTestBackendOn(t, newPairedBackend(t), readiness...)
}

// transferTestBackendOn is transferTestBackend on a backend the test built,
// for one that needs a second handle on its database.
func transferTestBackendOn(t *testing.T, backend *pairedBackend, readiness ...func(context.Context, string) error) *App {
	t.Helper()
	a := backend.app
	// The fixture clones a migrated database template. Give each transport its
	// own published identity, as independent production installations have.
	a.storeIdentity.Store(&store.Identity{BackendID: uuid.NewString(), ReplicaGeneration: uuid.NewString()})
	a.configDir = t.TempDir()
	a.git = gitops.NewCore()
	check := func(context.Context, string) error { return nil }
	if len(readiness) > 0 {
		check = readiness[0]
	}
	a.providerDiscoveryOnce.Do(func() {
		a.providerDiscovery = providerdiscoveryapp.New(providerdiscoveryapp.Deps{
			ProviderBinary: func(string) string { return "mock-provider" },
			DetectProvider: func(name, binary string) provider.ProviderStatus {
				return provider.ProviderStatus{Provider: name, BinaryPath: binary, Status: "ready", Installed: true, Version: "99.0.0"}
			},
			CheckClaudeTransferAccount: check,
			CheckCodexTransferAccount:  check,
		}, nil)
	})
	var err error
	a.attachments, err = attachment.NewStore(attachment.Config{RootDir: filepath.Join(a.configDir, "attachments")}, a.store)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.startThreadTransfers(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.appCancel(); a.transfers.close() })
	return a
}

func TestConversationTransferUsesHostJobsAndNativeSessions(t *testing.T) {
	for _, kind := range []string{"copy", "move"} {
		for _, fork := range []bool{false, true} {
			label := kind
			if fork {
				label += " pinned fork"
			}
			t.Run(label, func(t *testing.T) { testConversationTransfer(t, kind, fork, false) })
		}
	}
}

func TestConversationTransferWaitsForDestinationAccountBeforeRetiringSource(t *testing.T) {
	testConversationTransfer(t, "move", false, true)
}

func testConversationTransfer(t *testing.T, kind string, fork, initiallyUnavailable bool) {
	var ready atomic.Bool
	ready.Store(!initiallyUnavailable)
	source, destination := transferTestBackend(t), transferTestBackend(t, func(context.Context, string) error {
		if !ready.Load() {
			return errors.New("Sign in to Claude on the destination.")
		}
		return nil
	})
	ctx := context.Background()
	sourcePath := testutil.InitGitRepo(t)
	if err := os.WriteFile(filepath.Join(sourcePath, "notes.txt"), []byte("staged content"), 0o600); err != nil {
		t.Fatal(err)
	}
	testutil.RunGit(t, sourcePath, "add", "notes.txt")
	if err := os.WriteFile(filepath.Join(sourcePath, "notes.txt"), []byte("working content"), 0o600); err != nil {
		t.Fatal(err)
	}
	sourceProject, err := source.store.CreateProject(store.Project{ID: uuid.NewString(), Name: "Source", Path: sourcePath})
	if err != nil {
		t.Fatal(err)
	}
	destProject, err := destination.store.CreateProject(store.Project{ID: uuid.NewString(), Name: "Destination", Path: testutil.InitGitRepo(t)})
	if err != nil {
		t.Fatal(err)
	}
	thread := store.Thread{ID: uuid.NewString(), ProjectID: sourceProject.ID, Title: "Portable history", Provider: "claude", RuntimeMode: string(provider.RuntimeApprovalRequired), SessionRef: uuid.NewString(), WorkspacePath: sourcePath, CreatedAt: 1, UpdatedAt: 1}
	if err := source.store.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	if err := source.store.InsertItem(store.Item{ID: "message", ThreadID: thread.ID, Kind: "user_text", Role: "user", Status: "completed", Summary: "hello native history", Meta: "{}"}); err != nil {
		t.Fatal(err)
	}
	projects, err := source.claudeProjectsDir()
	if err != nil {
		t.Fatal(err)
	}
	nativeDir, err := sessionfork.WorkspaceProjectDir(projects, sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(nativeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	nativeID, pin := thread.SessionRef, uuid.NewString()
	native := []byte(`{"type":"user","sessionId":"` + nativeID + `","uuid":"` + pin + `","parentUuid":null,"message":{"role":"user","content":"hello native history"}}` + "\n")
	var parent store.Thread
	if fork {
		parent = thread
		parent.ID = uuid.NewString()
		if err := source.store.CreateThread(parent); err != nil {
			t.Fatal(err)
		}
		thread.SessionRef, thread.PendingForkRef, thread.PendingForkResumeAt = "", nativeID, pin
		if err := source.store.SetThreadForkResume(thread.ID, "", nativeID, pin); err != nil {
			t.Fatal(err)
		}
		native = append(native, []byte(`{"type":"user","sessionId":"`+nativeID+`","uuid":"`+uuid.NewString()+`","parentUuid":"`+pin+`","message":{"role":"user","content":"parent future must stay behind"}}`+"\n")...)
	}
	if err := os.WriteFile(filepath.Join(nativeDir, nativeID+".jsonl"), native, 0o600); err != nil {
		t.Fatal(err)
	}
	destinationID, _ := destination.backendIdentity()
	operation := uuid.NewString()
	intent, err := source.BeginThreadTransfer(ctx, thread.ID, operation, destinationID, kind, true)
	if err != nil {
		t.Fatal(err)
	}
	if again, err := source.BeginThreadTransfer(ctx, thread.ID, operation, destinationID, kind, true); err != nil || again != intent {
		t.Fatalf("lost Begin reply retry: %+v %v", again, err)
	}
	workspace := filepath.Join(t.TempDir(), "copied")
	offer, err := destination.CreateThreadTransferOffer(ctx, intent, destProject.ID, workspace, "copied-history")
	if err != nil {
		t.Fatal(err)
	}
	if again, err := destination.CreateThreadTransferOffer(ctx, intent, destProject.ID, workspace, "copied-history"); err != nil || again != offer {
		t.Fatalf("lost offer reply retry: %+v %v", again, err)
	}
	frontend, cancel := context.WithCancel(ctx)
	if _, err := source.BindThreadTransferDestination(frontend, thread.ID, offer); err != nil {
		t.Fatal(err)
	}
	cancel() // The phone can now disconnect; jobs use each host's lifetime.
	if initiallyUnavailable {
		deadline := time.Now().Add(15 * time.Second)
		for {
			pending, err := destination.store.GetThreadTransfer(operation)
			if err != nil {
				t.Fatal(err)
			}
			if pending.Error != "" {
				if pending.Phase != "preparing" || !strings.Contains(pending.Error, "Sign in") {
					t.Fatal("unexpected refusal", pending.Phase, pending.Error)
				}
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("destination account refusal was never reported")
			}
			time.Sleep(20 * time.Millisecond)
		}
		pending, err := source.store.GetThreadTransfer(operation)
		if err != nil || pending.Phase != "preparing" {
			t.Fatal("source retired before account admission", pending.Phase, err)
		}
		ready.Store(true)
		// An explicit retry keeps the same accepted snapshot and operation.
		if err := destination.RetryThreadTransfer(operation); err != nil {
			t.Fatal(err)
		}
		if err := source.RetryThreadTransfer(operation); err != nil {
			t.Fatal(err)
		}
	}
	awaitTransferPhase(t, source, operation, "complete", initiallyUnavailable)
	awaitTransferPhase(t, destination, operation, "complete", initiallyUnavailable)
	copied, err := destination.store.GetThread(intent.TargetThreadID)
	if err != nil {
		t.Fatal(err)
	}
	if copied.SessionRef == "" || (kind == "copy" && copied.ID == thread.ID) || (kind == "move" && copied.ID != thread.ID) || ((kind == "copy" || fork) && copied.SessionRef == nativeID) || (kind == "move" && !fork && copied.SessionRef != nativeID) {
		t.Fatalf("copy shared identity: %+v", copied)
	}
	if copied.WorkspacePath != workspace || copied.ProjectID != destProject.ID {
		t.Fatalf("wrong execution target: %+v", copied)
	}
	if err := source.store.CheckThreadExecutionAccess(thread); (err != nil) != (kind == "move") {
		t.Fatalf("original blocked after copy: %v", err)
	}
	if err := destination.store.CheckThreadExecutionAccess(copied); err != nil {
		t.Fatalf("copy not runnable: %v", err)
	}
	if fork {
		if copied.PendingForkRef != "" || copied.PendingForkResumeAt != "" {
			t.Fatal("transferred fork still depends on parent")
		}
		if err := source.store.CheckThreadExecutionAccess(parent); err != nil {
			t.Fatalf("fork transfer retired its parent: %v", err)
		}
	}
	data, err := os.ReadFile(filepath.Join(workspace, "notes.txt"))
	if err != nil || string(data) != "working content" {
		t.Fatalf("working bytes: %q %v", data, err)
	}
	staged, _, err := destination.git.Execute(workspace, "show", ":notes.txt")
	if err != nil || staged != "staged content" {
		t.Fatalf("staged bytes: %q %v", staged, err)
	}
	destNative, err := destination.claudeProjectsDir()
	if err != nil {
		t.Fatal(err)
	}
	file, err := sessionfork.LocateSessionFile(destNative, copied.SessionRef, workspace)
	if err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), copied.SessionRef) || ((kind == "copy" || fork) && strings.Contains(string(data), nativeID)) {
		t.Fatalf("native identity was not independent: %s", data)
	}
	if fork && strings.Contains(string(data), "parent future must stay behind") {
		t.Fatal("fork transfer included later parent history")
	}
	if kind == "move" {
		if !fork {
			// Cleanup removes only the old local cache. A later return can
			// reconstruct it without reviving the retired native identity.
			unlock := source.threadLocks().Lock(thread.ID)
			err := source.deleteThreadTreeLocked(thread.ID)
			unlock()
			if err != nil {
				t.Fatal(err)
			}
			if err := source.store.CheckThreadExecutionAccess(thread); err == nil {
				t.Fatal("cache deletion revived execution")
			}
		}
		// Continue on B, then return to A. A retains a fenced older native
		// file; only that proven retired identity may accept these new bytes.
		data = append(data, []byte(`{"type":"user","sessionId":"`+copied.SessionRef+`","uuid":"`+uuid.NewString()+`","message":{"role":"user","content":"continued on B"}}`+"\n")...)
		if err := os.WriteFile(file, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := destination.store.InsertItem(store.Item{ID: "second-message", ThreadID: copied.ID, Kind: "user_text", Role: "user", Status: "completed", Summary: "continued on B", Meta: "{}", ItemIndex: 1}); err != nil {
			t.Fatal(err)
		}
		originalID, _ := source.backendIdentity()
		returnOp := uuid.NewString()
		back, err := destination.BeginThreadTransfer(ctx, copied.ID, returnOp, originalID, "move", false)
		if err != nil {
			t.Fatal(err)
		}
		returnOffer, err := source.CreateThreadTransferOffer(ctx, back, sourceProject.ID, sourcePath, "")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := destination.BindThreadTransferDestination(ctx, copied.ID, returnOffer); err != nil {
			t.Fatal(err)
		}
		awaitTransferPhase(t, destination, returnOp, "complete")
		awaitTransferPhase(t, source, returnOp, "complete")
		returned, err := source.store.GetThread(thread.ID)
		if err != nil {
			t.Fatal(err)
		}
		if returned.OwnershipEpoch <= copied.OwnershipEpoch {
			t.Fatal("return did not advance execution ownership")
		}
		if err := source.store.CheckThreadExecutionAccess(returned); err != nil {
			t.Fatalf("returned conversation blocked: %v", err)
		}
		if err := destination.store.CheckThreadExecutionAccess(copied); err == nil {
			t.Fatal("previous owner remained runnable after return")
		}
		current, err := os.ReadFile(filepath.Join(nativeDir, copied.SessionRef+".jsonl"))
		if err != nil || !strings.Contains(string(current), "continued on B") {
			t.Fatalf("return lost new native history: %q %v", current, err)
		}
	}
	statuses, err := source.GetThreadTransfers()
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(statuses)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), offer.Grant) || strings.Contains(string(encoded), intent.ActivationHash) {
		t.Fatal("public status leaked authorization material")
	}
}

func awaitTransferPhase(t *testing.T, a *App, id, phase string, recovering ...bool) store.ThreadTransfer {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		row, err := a.store.GetThreadTransfer(id)
		if err != nil {
			t.Fatal(err)
		}
		if row.Phase == phase {
			return row
		}
		if row.Error != "" && !(len(recovering) > 0 && recovering[0]) {
			t.Fatalf("transfer %s (%s): %s", row.Direction, row.Phase, row.Error)
		}
		time.Sleep(20 * time.Millisecond)
	}
	row, _ := a.store.GetThreadTransfer(id)
	t.Fatalf("timed out awaiting %s: %+v", phase, row)
	return row
}

// TestPointerForkTransferDuringASourceDelete: when the source's delete
// begins between two batches of a pointer fork's export, the attempt fails
// with the public sentence, which the transfer status shows, and the fork
// reads what it read. Once the delete has finished, the retried attempt
// transfers the fork without the source's messages.
func TestPointerForkTransferDuringASourceDelete(t *testing.T) {
	app, dbPath := newTestAppWithStorePath(t)
	if err := app.initIdentity("backend-under-test"); err != nil {
		t.Fatal(err)
	}
	source, destination := transferTestBackendOn(t, servePairedApp(t, app)), transferTestBackend(t)
	ctx := context.Background()
	sourcePath, destinationPath := testutil.InitGitRepo(t), testutil.InitGitRepo(t)
	sourceProject, err := source.store.CreateProject(store.Project{ID: uuid.NewString(), Name: "Source", Path: sourcePath})
	if err != nil {
		t.Fatal(err)
	}
	destProject, err := destination.store.CreateProject(store.Project{ID: uuid.NewString(), Name: "Destination", Path: destinationPath})
	if err != nil {
		t.Fatal(err)
	}
	origin := store.Thread{ID: uuid.NewString(), ProjectID: sourceProject.ID, Title: "Origin", Provider: "claude", RuntimeMode: string(provider.RuntimeApprovalRequired), WorkspacePath: sourcePath, CreatedAt: 1, UpdatedAt: 1}
	if err := source.store.CreateThread(origin); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	// 600 rows: the export's first batch copies 500 of them.
	if _, err := raw.Exec(`WITH RECURSIVE n(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM n WHERE i < 599)
		INSERT INTO items(thread_id,id,turn_index,item_index,kind,role,status,summary,meta,created_at,updated_at)
		SELECT ?, 'origin-' || i, i / 10, i % 10, 'assistant_text', 'assistant', 'completed', 'origin row', '{}', 1, 1 FROM n`, origin.ID); err != nil {
		t.Fatal(err)
	}
	fork := origin
	fork.ID, fork.Title, fork.ForkedFromThreadID, fork.SessionRef, fork.CreatedAt, fork.UpdatedAt = uuid.NewString(), "Fork", origin.ID, uuid.NewString(), 2, 2
	if err := source.store.CreatePointerFork(fork, origin.ID, store.ForkCut{}, func(s string) string { return s }, 1); err != nil {
		t.Fatal(err)
	}
	projects, err := source.claudeProjectsDir()
	if err != nil {
		t.Fatal(err)
	}
	nativeDir, err := sessionfork.WorkspaceProjectDir(projects, sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(nativeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	native := `{"type":"user","sessionId":"` + fork.SessionRef + `","uuid":"` + uuid.NewString() + `","parentUuid":null,"message":{"role":"user","content":"fork only"}}` + "\n"
	if err := os.WriteFile(filepath.Join(nativeDir, fork.SessionRef+".jsonl"), []byte(native), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := source.store.InsertItem(store.Item{ID: "fork-own", ThreadID: fork.ID, TurnIndex: 100, Kind: "user_text", Role: "user", Status: "completed", Summary: "fork only", Meta: "{}"}); err != nil {
		t.Fatal(err)
	}
	forkIDs := func() []string {
		t.Helper()
		items, err := source.store.ListItems(fork.ID)
		if err != nil {
			t.Fatal(err)
		}
		ids := make([]string, len(items))
		for i, item := range items {
			ids[i] = item.ID
		}
		return ids
	}
	before := forkIDs()
	if len(before) != 602 {
		t.Fatalf("the fork reads %d rows, want the origin's 600, its divider and its own", len(before))
	}
	// The delete's mark, committed with the first batch's copies.
	if _, err := raw.Exec(`CREATE TRIGGER test_delete_begins_mid_export AFTER INSERT ON thread_fork_copied
		WHEN (SELECT count(*) FROM thread_fork_copied WHERE thread_id = NEW.thread_id) = 500
		BEGIN UPDATE threads SET deleting = 1 WHERE id = '` + origin.ID + `'; END`); err != nil {
		t.Fatal(err)
	}

	destinationID, _ := destination.backendIdentity()
	operation := uuid.NewString()
	intent, err := source.BeginThreadTransfer(ctx, fork.ID, operation, destinationID, "copy", false)
	if err != nil {
		t.Fatal(err)
	}
	offer, err := destination.CreateThreadTransferOffer(ctx, intent, destProject.ID, destinationPath, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.BindThreadTransferDestination(ctx, fork.ID, offer); err != nil {
		t.Fatal(err)
	}
	const refusal = "A conversation this one was forked from is being deleted. The transfer will continue without its messages once the delete finishes."
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		statuses, err := source.GetThreadTransfers()
		if err != nil {
			t.Fatal(err)
		}
		var status store.ThreadTransfer
		for _, row := range statuses {
			if row.ID == operation {
				status = row
			}
		}
		if status.Error == refusal {
			break
		}
		if status.Error != "" || status.Phase == "complete" || time.Now().After(deadline) {
			t.Fatalf("transfer status = %s %q, want the refusal", status.Phase, status.Error)
		}
	}
	if got := forkIDs(); !slices.Equal(got, before) {
		t.Fatalf("the refused export changed the fork to %d rows", len(got))
	}

	if _, err := raw.Exec(`DROP TRIGGER test_delete_begins_mid_export`); err != nil {
		t.Fatal(err)
	}
	if err := source.DeleteThread(origin.ID); err != nil {
		t.Fatal(err)
	}
	if got, want := forkIDs(), []string{"fork-origin-" + fork.ID, "fork-own"}; !slices.Equal(got, want) {
		t.Fatalf("fork after the origin's delete = %v, want %v", got, want)
	}
	var records int
	if err := raw.QueryRow(`SELECT count(*) FROM thread_fork_copied`).Scan(&records); err != nil || records != 0 {
		t.Fatalf("records after the origin's delete = %d, %v", records, err)
	}
	if err := source.RetryThreadTransfer(operation); err != nil {
		t.Fatal(err)
	}
	awaitTransferPhase(t, source, operation, "complete", true)
	awaitTransferPhase(t, destination, operation, "complete", true)
	copied, err := destination.store.ListItems(intent.TargetThreadID)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, item := range copied {
		ids = append(ids, item.ID)
	}
	if !slices.Contains(ids, "fork-own") || slices.ContainsFunc(ids, func(id string) bool { return strings.HasPrefix(id, "origin-") }) {
		t.Fatalf("the transferred fork holds %v, want its own rows only", ids)
	}
}

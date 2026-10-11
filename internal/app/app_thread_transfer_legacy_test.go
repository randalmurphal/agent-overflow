package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"agent-overflow/internal/buildvariant/remotetest"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude/sessionfork"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/transferfiles"
	"github.com/google/uuid"
)

// TestTransferredDiscussionInstallsAsChat: a peer from before migration v142
// sends a former discussion thread with mode 'discussion' and the discussion
// keys v142 dropped, under manifest version 1. The receiver installs it as a
// chat, as v142 did with its stored rows. A recipe that receiver prepared
// before its own update to v142 still holds 'discussion' and installs the
// same way.
func TestTransferredDiscussionInstallsAsChat(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	for _, preparedBeforeUpdate := range []bool{false, true} {
		name := "manifest from an older sender"
		if preparedBeforeUpdate {
			name = "recipe prepared before the update"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			testTransferredDiscussionInstallsAsChat(t, preparedBeforeUpdate)
		})
	}
}

func testTransferredDiscussionInstallsAsChat(t *testing.T, preparedBeforeUpdate bool) {
	source, destination := transferTestBackend(t), transferTestBackend(t)
	// The test runs the destination's preparation itself, on a manifest it
	// rewrites the way an older sender writes it.
	destination.transfers.live.Load().jobs.Close()
	incoming := destination.transfers.live.Load().destination
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
	thread := store.Thread{ID: uuid.NewString(), ProjectID: sourceProject.ID, Title: "Former discussion", Provider: "claude", RuntimeMode: string(provider.RuntimeApprovalRequired), SessionRef: uuid.NewString(), WorkspacePath: sourcePath, CreatedAt: 1, UpdatedAt: 1}
	if err := source.store.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	if err := source.store.InsertItem(store.Item{ID: "message", ThreadID: thread.ID, Kind: "user_text", Role: "user", Status: "completed", Summary: "hello", Meta: "{}"}); err != nil {
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
	native := `{"type":"user","sessionId":"` + thread.SessionRef + `","uuid":"` + uuid.NewString() + `","parentUuid":null,"message":{"role":"user","content":"hello"}}` + "\n"
	if err := os.WriteFile(filepath.Join(nativeDir, thread.SessionRef+".jsonl"), []byte(native), 0o600); err != nil {
		t.Fatal(err)
	}

	destinationID, _ := destination.backendIdentity()
	operation := uuid.NewString()
	intent, err := source.BeginThreadTransfer(ctx, thread.ID, operation, destinationID, "copy", false)
	if err != nil {
		t.Fatal(err)
	}
	offer, err := destination.CreateThreadTransferOffer(ctx, intent, destProject.ID, destinationPath, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.BindThreadTransferDestination(ctx, thread.ID, offer); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(destination.configDir, "conversation-transfers", operation)
	archive := awaitTransferredArchive(t, source, directory, operation)

	row, err := destination.store.GetThreadTransfer(operation)
	if err != nil {
		t.Fatal(err)
	}
	stage := filepath.Join(directory, "extracted")
	input, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	files, err := transferfiles.Extract(ctx, input, row.ManifestHash, stage)
	if closeErr := input.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
	rewriteTransferJSON(t, filepath.Join(stage, "manifest.json"), func(manifest map[string]any) {
		older := manifest["thread"].(map[string]any)
		older["mode"], older["discussionId"], older["parentThreadId"] = "discussion", uuid.NewString(), uuid.NewString()
	})
	recipe, err := appTransferInstaller{destination}.Prepare(ctx, row, stage, files)
	if err != nil {
		t.Fatalf("prepare a version 1 manifest of a former discussion: %v", err)
	}
	var plan transferInstallPlan
	if err := json.Unmarshal(recipe, &plan); err != nil {
		t.Fatal(err)
	}
	if plan.Thread.Mode != "chat" {
		t.Fatalf("prepared mode = %q, want chat", plan.Thread.Mode)
	}
	prepared := filepath.Join(directory, "prepared.json")
	if err := os.WriteFile(prepared, recipe, 0o600); err != nil {
		t.Fatal(err)
	}
	if preparedBeforeUpdate {
		rewriteTransferJSON(t, prepared, func(recipe map[string]any) {
			recipe["thread"].(map[string]any)["mode"] = "discussion"
		})
	}
	if _, err := destination.store.AdvanceThreadTransfer(operation, "prepared", row.ManifestHash); err != nil {
		t.Fatal(err)
	}
	awaitTransferActivation(t, source, directory, operation)
	if _, err := incoming.Run(ctx, operation); err != nil {
		t.Fatalf("install a former discussion: %v", err)
	}
	installed, err := destination.store.GetThread(intent.TargetThreadID)
	if err != nil {
		t.Fatal(err)
	}
	if installed.Mode != "chat" {
		t.Fatalf("installed mode = %q, want chat", installed.Mode)
	}
	awaitTransferComplete(t, source, destination, operation, false)
}

// awaitTransferredArchive waits until the source has uploaded every byte of
// the archive and returns its path on the destination.
func awaitTransferredArchive(t *testing.T, source *App, directory, operation string) string {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		archive, _, err := transferfiles.UploadedArchive(directory)
		if err == nil {
			return archive
		}
		if time.Now().After(deadline) {
			t.Fatalf("the source never finished its upload: %v", err)
		}
		source.transfers.wake(operation)
		time.Sleep(100 * time.Millisecond)
	}
}

// awaitTransferActivation waits until the source has seen the prepared
// destination, committed and sent its activation proof.
func awaitTransferActivation(t *testing.T, source *App, directory, operation string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		_, err := os.Stat(filepath.Join(directory, "activation.json"))
		if err == nil {
			return
		}
		if !errors.Is(err, os.ErrNotExist) || time.Now().After(deadline) {
			t.Fatalf("the source never activated the destination: %v", err)
		}
		source.transfers.wake(operation)
		time.Sleep(100 * time.Millisecond)
	}
}

func rewriteTransferJSON(t *testing.T, path string, change func(map[string]any)) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Numbers stay exact; the rewrite changes only the fields it names.
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value map[string]any
	if err := decoder.Decode(&value); err != nil {
		t.Fatal(err)
	}
	change(value)
	if data, err = json.Marshal(value); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

package app

import (
	"os/exec"
	"strings"
	"testing"

	"agent-overflow/internal/store"
	"agent-overflow/internal/transport"
)

// The device id travels on the connection, not in any method's arguments, so
// the only thing that can carry it into a bound method is the call context.
// This is the end-to-end proof of that conduit for thread creation.
func TestACreatedThreadRemembersTheScreenThatStartedIt(t *testing.T) {
	app := newTestApp(t)
	project, err := app.ensureProjectForWorkspace(t.TempDir())
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace: %v", err)
	}
	ctx := ctxFromClient(transport.ClientIdentity{DeviceID: "laptop-1", ConnectionID: "conn-9"})

	thread, err := app.CreateThread(ctx, CreateThreadOptions{ProjectID: project.ID})
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	if thread.CreatedByDevice != "laptop-1" {
		t.Fatalf("createdByDevice = %q, want laptop-1", thread.CreatedByDevice)
	}
}

// The connection id is per page load; the device id outlives it. Recording the
// connection would make the attribution expire the moment the tab reloads,
// which defeats the purpose of persisting it at all.
func TestCreationAttributionIsTheDeviceNotTheConnection(t *testing.T) {
	app := newTestApp(t)
	project, err := app.ensureProjectForWorkspace(t.TempDir())
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace: %v", err)
	}
	ctx := ctxFromClient(transport.ClientIdentity{DeviceID: "laptop-1", ConnectionID: "conn-9"})

	thread, err := app.CreateThread(ctx, CreateThreadOptions{ProjectID: project.ID})
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	if thread.CreatedByDevice == "conn-9" {
		t.Fatal("createdByDevice recorded the connection id; it must record the device id")
	}
}

// An in-process caller — a background saga, the harness RPC, a test — has no
// screen behind it. Empty is the correct answer, not a failure.
func TestABackendCreatedThreadIsAttributedToNoDevice(t *testing.T) {
	app := newTestApp(t)
	thread, err := createTestThread(t, app, "claude", t.TempDir(), "claude-sonnet-4-6", "chat")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}
	if thread.CreatedByDevice != "" {
		t.Fatalf("createdByDevice = %q, want empty", thread.CreatedByDevice)
	}
}

// observeThreadOrigin runs against real git, so a temp directory that is not a
// repository is the honest "nothing known" case — and the one that must not
// error, since plenty of real workspaces are exactly this.
func TestObservingANonRepositoryWorkspaceReportsNothingKnown(t *testing.T) {
	app := newTestApp(t)
	if origin := app.observeThreadOrigin(t.TempDir()); !origin.IsZero() {
		t.Fatalf("origin = %+v, want zero for a directory that is not a repository", origin)
	}
	if origin := app.observeThreadOrigin(""); !origin.IsZero() {
		t.Fatalf("origin = %+v, want zero for an empty workspace path", origin)
	}
}

// stampThreadCreation is the single entry point every creation path uses, so
// its contract — fill both, overwrite whatever was there — is worth pinning.
func TestStampingCreationFactsFillsBothAndToleratesANilThread(t *testing.T) {
	app := newTestApp(t)
	thread := store.Thread{WorkspacePath: t.TempDir(), CreatedByDevice: "stale"}
	app.stampThreadCreation(ctxFromClient(transport.ClientIdentity{DeviceID: "desk-2"}), &thread)
	if thread.CreatedByDevice != "desk-2" {
		t.Fatalf("createdByDevice = %q, want desk-2", thread.CreatedByDevice)
	}
	if !thread.Origin.IsZero() {
		t.Fatalf("origin = %+v, want zero for a directory that is not a repository", thread.Origin)
	}
	app.stampThreadCreation(t.Context(), nil) // must not panic
}

// The real read, against a real repository: branch, remote, and head are the
// three coordinates a transfer needs, and each comes from a different git
// command, so a test that stubs them proves nothing about whether they work.
func TestObservingARepositoryRecordsBranchRemoteAndHead(t *testing.T) {
	app := newTestApp(t)
	repo := initCommitMsgRepo(t)
	remote := "git@example.com:owner/repo.git"
	cmd := exec.Command("git", "remote", "add", "origin", remote)
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git remote add: %v\n%s", err, out)
	}

	origin := app.observeThreadOrigin(repo)
	if origin.Branch != "main" {
		t.Fatalf("branch = %q, want main", origin.Branch)
	}
	if origin.RemoteURL != remote {
		t.Fatalf("remoteUrl = %q, want %q", origin.RemoteURL, remote)
	}
	if len(strings.TrimSpace(origin.HeadCommit)) < 7 {
		t.Fatalf("headCommit = %q, want a commit sha", origin.HeadCommit)
	}
}

// A repository with no `origin` is ordinary — a local-only repo, a clone whose
// remote was renamed. The other two coordinates must still be recorded.
func TestARepositoryWithNoRemoteStillRecordsBranchAndHead(t *testing.T) {
	app := newTestApp(t)
	origin := app.observeThreadOrigin(initCommitMsgRepo(t))
	if origin.RemoteURL != "" {
		t.Fatalf("remoteUrl = %q, want empty", origin.RemoteURL)
	}
	if origin.Branch != "main" || origin.HeadCommit == "" {
		t.Fatalf("branch = %q head = %q; both should still be known", origin.Branch, origin.HeadCommit)
	}
}

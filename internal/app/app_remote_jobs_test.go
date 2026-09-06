package app

import (
	"context"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/attachedbackends"
	"agent-overflow/internal/gitapp"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/remotejobs"
	"agent-overflow/internal/store"
	"agent-overflow/internal/transport"
	"github.com/google/uuid"
)

func TestAgentRemoteCommandUsesItsOwnPairedIdentityAndSurvivesSourceLoss(t *testing.T) {
	backend := newPairedBackend(t)
	source := identityApp(t)
	manager, err := attachedbackends.New(t.TempDir(), "workhorse", "linux")
	if err != nil {
		t.Fatal(err)
	}
	source.backends = manager
	invite, link := backend.mintLink(t, "full")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	attached, err := manager.Add(ctx, invite.URL)
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.app.ConfirmDevicePairing(invite.LinkID); err != nil {
		t.Fatal(err)
	}
	if err := manager.Await(ctx, attached.ID); err != nil {
		t.Fatal(err)
	}
	if attached.ID != link.BackendID {
		t.Fatal("paired identity drift")
	}
	project, err := backend.app.store.CreateProject(store.Project{ID: uuid.NewString(), Name: "GPU repo", Path: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	started, finish := make(chan struct{}), make(chan struct{})
	var executions atomic.Int32
	backend.app.remoteJobs, err = remotejobs.New(context.Background(), backend.app.store, func(ctx context.Context, cwd string, argv []string, out io.Writer) (int, error) {
		executions.Add(1)
		close(started)
		if cwd != project.Path || strings.Join(argv, " ") != "train --gpu" {
			t.Errorf("destination: %s %v", cwd, argv)
		}
		select {
		case <-ctx.Done():
			return -1, ctx.Err()
		case <-finish:
		}
		_, _ = io.WriteString(out, "GPU result")
		return 0, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(backend.app.remoteJobs.Close)
	thread := uuid.NewString()
	caller := transport.WithCallerScope(ctx, transport.CallerScope{Kind: transport.ScopeKindInteractive, ThreadID: thread, ProjectID: uuid.NewString()})
	input := AgentRemoteRequest{ComputerID: attached.ID, Workspace: gitapp.WorkspaceRef{ProjectID: project.ID}, Request: remotejobs.Request{ID: uuid.NewString(), SourceThreadID: uuid.NewString(), Argv: []string{"train", "--gpu"}, TimeoutSeconds: 60}}
	if _, err := source.AgentRemoteStart(caller, input); err == nil {
		t.Fatal("pairing implicitly enabled agent execution")
	}
	if err := source.SetAgentComputerEnabled(context.Background(), attached.ID, true); err != nil {
		t.Fatal(err)
	}
	computers, err := source.AgentRemoteComputers(caller)
	if err != nil || len(computers) != 1 || computers[0].Error != "" || len(computers[0].Projects) < 1 {
		t.Fatalf("computers: %#v %v", computers, err)
	}
	result, err := source.AgentRemoteStart(caller, input)
	if err != nil {
		t.Fatal(err)
	}
	<-started
	if result.SourceThreadID != thread {
		t.Fatal("caller-controlled source identity")
	}
	if retry, err := source.AgentRemoteStart(caller, input); err != nil || retry.ID != result.ID {
		t.Fatalf("retry: %#v %v", retry, err)
	}
	other := transport.WithCallerScope(ctx, transport.CallerScope{Kind: transport.ScopeKindInteractive, ThreadID: uuid.NewString()})
	if _, err := source.AgentRemoteCancel(other, attached.ID, result.ID); err == nil {
		t.Fatal("another thread canceled command")
	}
	if err := source.SetAgentComputerEnabled(context.Background(), attached.ID, false); err != nil {
		t.Fatal(err)
	}
	close(finish)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		got, err := source.AgentRemoteStatus(caller, attached.ID, result.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.State == "succeeded" {
			if got.Output != "GPU result" || executions.Load() != 1 {
				t.Fatalf("result: %#v", got)
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("command did not finish after originating RPC closed and access was disabled")
}

func TestAgentRemoteEnableChecksPairingAndDestinationScope(t *testing.T) {
	for _, access := range []string{"full", "view-only"} {
		t.Run(access, func(t *testing.T) {
			backend := newPairedBackend(t)
			source := identityApp(t)
			manager, err := attachedbackends.New(t.TempDir(), "test source", "linux")
			if err != nil {
				t.Fatal(err)
			}
			source.backends = manager
			invite, _ := backend.mintLink(t, access)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			peer, err := manager.Add(ctx, invite.URL)
			if err != nil {
				t.Fatal(err)
			}
			if err := source.SetAgentComputerEnabled(ctx, peer.ID, true); err == nil {
				t.Fatal("enabled an unconfirmed pairing")
			}
			if err := backend.app.ConfirmDevicePairing(invite.LinkID); err != nil {
				t.Fatal(err)
			}
			if err := manager.Await(ctx, peer.ID); err != nil {
				t.Fatal(err)
			}
			err = source.SetAgentComputerEnabled(ctx, peer.ID, true)
			if access == "view-only" {
				if err == nil {
					t.Fatal("enabled a peer without command scope")
				}
				enabled, readErr := manager.AgentAccess()
				if readErr != nil || enabled[peer.ID] {
					t.Fatal("failed enable persisted opt-in", readErr)
				}
				return
			}
			if err != nil {
				t.Fatal("confirmed pairing cannot recover:", err)
			}
			repairInvite, _ := backend.mintLink(t, "full")
			if _, err := manager.Add(ctx, repairInvite.URL); err != nil {
				t.Fatal(err)
			}
			enabled, err := manager.AgentAccess()
			if err != nil || enabled[peer.ID] {
				t.Fatal("re-pair revived an earlier opt-in", err)
			}
			if err := backend.app.ConfirmDevicePairing(repairInvite.LinkID); err != nil {
				t.Fatal(err)
			}
			if err := manager.Await(ctx, peer.ID); err != nil {
				t.Fatal(err)
			}
			if err := source.SetAgentComputerEnabled(ctx, peer.ID, true); err != nil {
				t.Fatal(err)
			}
			overview, err := backend.app.GetAccessOverview()
			if err != nil {
				t.Fatal(err)
			}
			revoked := false
			for _, device := range overview.Devices {
				if device.Label == "test source" {
					if _, err := backend.app.RevokeAccessDevice(device.ID); err != nil {
						t.Fatal(err)
					}
					revoked = true
				}
			}
			if !revoked {
				t.Fatal("paired device missing")
			}
			if err := source.SetAgentComputerEnabled(ctx, peer.ID, true); err == nil {
				t.Fatal("revoked pairing enabled")
			}
			if err := source.SetAgentComputerEnabled(ctx, peer.ID, false); err != nil {
				t.Fatal("forgotten revoked peer cannot be disabled:", err)
			}
		})
	}
}

func TestAgentRemoteDiscoveryAddsAndRemovesProviderGuidance(t *testing.T) {
	backend := newPairedBackend(t)
	source := identityApp(t)
	manager, err := attachedbackends.New(t.TempDir(), "test source", "linux")
	if err != nil {
		t.Fatal(err)
	}
	source.backends = manager
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	source.appCtx = ctx
	t.Cleanup(func() { cancel(); source.remotePeers.wg.Wait() })
	invite, _ := backend.mintLink(t, "full")
	peer, err := manager.Add(ctx, invite.URL)
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.app.ConfirmDevicePairing(invite.LinkID); err != nil {
		t.Fatal(err)
	}
	if err := manager.Await(ctx, peer.ID); err != nil {
		t.Fatal(err)
	}
	source.startRemotePeerDiscovery()
	thread := store.Thread{ID: uuid.NewString(), ProjectID: uuid.NewString(), Provider: string(provider.Claude)}
	if source.remoteInstructionsForThread(thread) != "" {
		t.Fatal("guidance appeared before opt-in")
	}
	if err := source.SetAgentComputerEnabled(ctx, peer.ID, true); err != nil {
		t.Fatal(err)
	}
	awaitReady := func(want bool) {
		t.Helper()
		for source.remotePeers.ready.Load() != want {
			select {
			case <-ctx.Done():
				t.Fatal("peer discovery did not converge", want)
			case <-time.After(5 * time.Millisecond):
			}
		}
	}
	awaitReady(true)
	for _, name := range []string{string(provider.Claude), string(provider.Codex)} {
		thread.Provider = string(name)
		if !strings.Contains(source.remoteInstructionsForThread(thread), "remote run") {
			t.Fatal("missing peer guidance", name)
		}
	}
	thread.Provider = "claude-tui"
	if source.remoteInstructionsForThread(thread) != "" {
		t.Fatal("unsupported provider got command guidance")
	}
	if err := source.SetAgentComputerEnabled(ctx, peer.ID, false); err != nil {
		t.Fatal(err)
	}
	awaitReady(false)
	thread.Provider = string(provider.Claude)
	if source.remoteInstructionsForThread(thread) != "" {
		t.Fatal("guidance survived disabling the peer")
	}
}

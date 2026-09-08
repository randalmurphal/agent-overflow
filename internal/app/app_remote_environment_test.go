package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"agent-overflow/internal/attachedbackends"
	"agent-overflow/internal/store"
	"agent-overflow/internal/transport"
	"github.com/google/uuid"
)

func TestRemoteEnvironmentDistinguishesWSLAndOnlyLooksUpExecutables(t *testing.T) {
	for _, tc := range []struct {
		os, host, environment string
		wsl                   bool
	}{
		{"linux", "windows", "wsl", true}, {"linux", "linux", "native", false},
		{"darwin", "darwin", "native", true}, {"windows", "windows", "native", false},
	} {
		t.Run(tc.os+tc.environment, func(t *testing.T) {
			calls := map[string]bool{}
			result := remoteCommandEnvironment(tc.os, "arm64", tc.wsl, "Ubuntu", func(name string) (string, error) {
				if calls[name] {
					t.Fatalf("duplicate executable lookup: %s", name)
				}
				calls[name] = true
				if name == "python3" {
					return "/tools/python3", nil
				}
				return "", errors.New("not found")
			})
			if result.ExecutionOS != tc.os || result.HostOS != tc.host || result.Environment != tc.environment || result.Architecture != "arm64" {
				t.Fatalf("environment: %#v", result)
			}
			if len(result.Executables) != 1 || result.Executables["python3"] != "/tools/python3" {
				t.Fatalf("invented executable availability: %#v", result.Executables)
			}
			if (result.Distribution != "") != (tc.environment == "wsl") {
				t.Fatalf("distribution leaked outside WSL: %#v", result)
			}
		})
	}
}

func TestRemoteProjectDiscoveryReturnsLiveRegisteredWorktreesAndSurvivesScanFailure(t *testing.T) {
	app := identityApp(t)
	repo := initGitRepo(t)
	worktree := filepath.Join(t.TempDir(), "feature checkout")
	missing := filepath.Join(t.TempDir(), "deleted checkout")
	runGit(t, repo, "worktree", "add", "-b", "remote-feature", worktree)
	runGit(t, repo, "worktree", "add", "--detach", missing)
	if err := os.RemoveAll(missing); err != nil {
		t.Fatal(err)
	}
	project, err := app.store.CreateProject(store.Project{ID: uuid.NewString(), Name: "repo", Path: repo})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := app.RemoteCommandProjects(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var found *RemoteCommandProject
	for i := range rows {
		if rows[i].ID == project.ID {
			found = &rows[i]
		}
	}
	if found == nil || found.WorktreesError != "" || len(found.Worktrees) != 2 {
		t.Fatalf("discovery: %#v", found)
	}
	seen := false
	for _, wt := range found.Worktrees {
		if wt.Branch == "remote-feature" {
			seen = true
			if wt.HEAD == "" {
				t.Fatal("missing worktree HEAD")
			}
		}
		if wt.Path == missing {
			t.Fatal("advertised missing checkout")
		}
	}
	if !seen {
		t.Fatal("missing worktree branch")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rows, err = app.RemoteCommandProjects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.ID == project.ID && (row.Path != repo || row.WorktreesError == "") {
			t.Fatalf("failed optional scan erased project or hid failure: %#v", row)
		}
	}
}

type legacyRemoteDiscovery struct{}

func (*legacyRemoteDiscovery) RemoteCommandProjects() []RemoteCommandProject {
	return []RemoteCommandProject{{ID: "94b695e8-7a4d-4d2b-b54d-80b43f56a7e2", Name: "legacy repo", Path: "/legacy/repo"}}
}

func TestRemoteDiscoveryKeepsProjectsFromPeerWithoutEnvironmentRPC(t *testing.T) {
	backend := newPairedBackend(t, func(cfg *transport.Config) {
		dispatcher := transport.NewDispatcher()
		if _, err := dispatcher.Register(&legacyRemoteDiscovery{}, transport.RegisterOptions{Package: "main", TypeName: "App", AllowList: map[string]bool{"RemoteCommandProjects": true}}); err != nil {
			t.Fatal(err)
		}
		cfg.Dispatcher = dispatcher
	})
	source := identityApp(t)
	manager, err := attachedbackends.New(t.TempDir(), "source", "linux")
	if err != nil {
		t.Fatal(err)
	}
	source.backends = manager
	invite, _ := backend.mintLink(t, "full")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
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
	if err := source.SetAgentComputerEnabled(ctx, peer.ID, true); err != nil {
		t.Fatal(err)
	}
	rows := source.probeAgentComputers(ctx, []AgentComputer{{ID: peer.ID, Enabled: true}})
	if len(rows) != 1 || rows[0].Error != "" || rows[0].EnvironmentError != "" || rows[0].Environment != nil || len(rows[0].Projects) != 1 {
		t.Fatalf("legacy discovery: %#v", rows)
	}
}

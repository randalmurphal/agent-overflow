package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"agent-overflow/internal/attachedbackends"
	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/gitapp"
	"agent-overflow/internal/remotejobs"
	"agent-overflow/internal/store"
	"agent-overflow/internal/transport"
	"github.com/google/uuid"
)

func TestRemoteArtifactCopyIntegrityAndPartialCleanup(t *testing.T) {
	for _, mode := range []string{"success", "empty", "canceled", "changed", "same-metadata-change", "wrong-conversation"} {
		t.Run(mode, func(t *testing.T) {
			workspace, destination := t.TempDir(), t.TempDir()
			data := bytes.Repeat([]byte("report data\n"), remoteArtifactChunkBytes/3)
			if mode == "empty" {
				data = nil
			}
			filename := filepath.Join(workspace, "report.html")
			if err := os.WriteFile(filename, data, 0o600); err != nil {
				t.Fatal(err)
			}
			original, err := os.Stat(filename)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			result, err := receiveRemoteArtifact(ctx, destination, "report.html", "thread", func(ctx context.Context, request RemoteArtifactRequest) (RemoteArtifactChunk, error) {
				calls++
				if calls == 2 {
					switch mode {
					case "changed":
						if err := os.WriteFile(filename, []byte("changed"), 0o600); err != nil {
							t.Fatal(err)
						}
					case "same-metadata-change":
						replacement := bytes.Repeat([]byte("x"), len(data))
						if err := os.WriteFile(filename, replacement, 0o600); err != nil {
							t.Fatal(err)
						}
						if err := os.Chtimes(filename, original.ModTime(), original.ModTime()); err != nil {
							t.Fatal(err)
						}
					}
				}
				chunk, err := readRemoteArtifactChunk(ctx, workspace, request)
				chunk.SourceThreadID = "thread"
				if mode == "wrong-conversation" {
					chunk.SourceThreadID = "other"
				}
				if mode == "canceled" {
					cancel()
				}
				return chunk, err
			})
			if mode == "success" || mode == "empty" {
				if err != nil {
					t.Fatal(err)
				}
				copied, err := os.ReadFile(result.Path)
				if err != nil {
					t.Fatal(err)
				}
				digest := sha256.Sum256(data)
				if !bytes.Equal(copied, data) || result.Size != int64(len(data)) || result.SHA256 != hex.EncodeToString(digest[:]) || filepath.Base(result.Path) != "report.html" {
					t.Fatalf("artifact: %#v", result)
				}
			} else {
				if err == nil {
					t.Fatal("published invalid artifact")
				}
				entries, readErr := os.ReadDir(destination)
				if readErr != nil || len(entries) != 0 {
					t.Fatalf("partial artifact leaked: %v %v", entries, readErr)
				}
			}
		})
	}
}

func TestRemoteArtifactReadConfinesFilesAndBoundsRequests(t *testing.T) {
	workspace, outside := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(workspace, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "ok"), []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("ok", filepath.Join(workspace, "inside")); err != nil {
		t.Fatal(err)
	}
	for _, request := range []RemoteArtifactRequest{{Path: "../outside"}, {Path: filepath.Join(outside, "secret")}, {Path: "escape/secret"}, {Path: "."}, {Path: "ok", Offset: -1}, {Path: "ok", Offset: 1}, {Path: "ok", Stamp: "stale"}} {
		if _, err := readRemoteArtifactChunk(context.Background(), workspace, request); err == nil {
			t.Fatalf("accepted %#v", request)
		}
	}
	if chunk, err := readRemoteArtifactChunk(context.Background(), workspace, RemoteArtifactRequest{Path: "inside"}); err != nil || string(chunk.Data) != "ok" {
		t.Fatalf("confined symlink: %#v %v", chunk, err)
	}
	file, err := os.Create(filepath.Join(workspace, "huge"))
	if err != nil {
		t.Fatal(err)
	}
	if err = file.Truncate(remoteArtifactMaxBytes + 1); err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	_, err = readRemoteArtifactChunk(context.Background(), workspace, RemoteArtifactRequest{Path: "huge"})
	code, _, _ := errorsx.PublicDetails(err)
	if code != "remote_artifact_too_large" {
		t.Fatalf("large file: %v", err)
	}
}

type artifactCancelingReader struct{ cancel context.CancelFunc }

func (r artifactCancelingReader) Read(p []byte) (int, error) {
	r.cancel()
	clear(p)
	return len(p), nil
}

func TestRemoteArtifactHashStopsOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// An otherwise unending input proves the streaming hash loop observes
	// cancellation between reads instead of hashing the full size first.
	hash := sha256.New()
	n, err := io.Copy(hash, &remoteArtifactReader{ctx: ctx, reader: artifactCancelingReader{cancel}})
	if !errors.Is(err, context.Canceled) || n == 0 || n > 64<<10 {
		t.Fatalf("canceled hash read %d bytes: %v", n, err)
	}
}

func TestRemoteArtifactRetrievalUsesPairedOwnerAndConversationAfterOptOut(t *testing.T) {
	backend := newPairedBackend(t)
	source := identityApp(t)
	source.configDir = t.TempDir()
	manager, err := attachedbackends.New(t.TempDir(), "source", "linux")
	if err != nil {
		t.Fatal(err)
	}
	source.backends = manager
	invite, _ := backend.mintLink(t, "full")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	peer, err := manager.Add(ctx, invite.URL)
	if err != nil {
		t.Fatal(err)
	}
	if err = backend.app.ConfirmDevicePairing(invite.LinkID); err != nil {
		t.Fatal(err)
	}
	if err = manager.Await(ctx, peer.ID); err != nil {
		t.Fatal(err)
	}
	if err = source.SetAgentComputerEnabled(ctx, peer.ID, true); err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	expected := bytes.Repeat([]byte("paired result\n"), remoteArtifactChunkBytes/5)
	if err = os.WriteFile(filepath.Join(workspace, "report.txt"), expected, 0o600); err != nil {
		t.Fatal(err)
	}
	project, err := backend.app.store.CreateProject(store.Project{ID: uuid.NewString(), Name: "destination", Path: workspace})
	if err != nil {
		t.Fatal(err)
	}
	backend.app.remoteJobs, err = remotejobs.New(context.Background(), backend.app.store, func(context.Context, string, []string, io.Writer) (remotejobs.Outcome, error) {
		return remotejobs.Outcome{ExitCode: 0}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(backend.app.remoteJobs.Close)
	thread, _ := remoteMCPThread(t, source, "codex")
	caller := transport.WithCallerScope(ctx, transport.CallerScope{Kind: transport.ScopeKindInteractive, ThreadID: thread.ID, ProjectID: thread.ProjectID})
	receipt, err := source.AgentRemoteStart(caller, AgentRemoteRequest{ComputerID: peer.ID, Workspace: gitapp.WorkspaceRef{ProjectID: project.ID}, Request: RemoteCommandRequest{ID: uuid.NewString(), Argv: []string{"fixture"}, TimeoutSeconds: 60}})
	if err != nil {
		t.Fatal(err)
	}
	if err = source.SetAgentComputerEnabled(ctx, peer.ID, false); err != nil {
		t.Fatal(err)
	}
	result, err := source.AgentRemoteFetchArtifact(caller, peer.ID, receipt.ID, "report.txt")
	if err != nil {
		t.Fatal(err)
	}
	actual, err := os.ReadFile(result.Path)
	if err != nil || !bytes.Equal(actual, expected) {
		t.Fatalf("paired copy: %v", err)
	}
	other := transport.WithCallerScope(ctx, transport.CallerScope{Kind: transport.ScopeKindInteractive, ThreadID: uuid.NewString()})
	if _, err = source.AgentRemoteFetchArtifact(other, peer.ID, receipt.ID, "report.txt"); err == nil {
		t.Fatal("other conversation fetched artifact")
	}
	if _, err = backend.app.RemoteCommandArtifact(context.Background(), receipt.ID, RemoteArtifactRequest{Path: "report.txt"}); err == nil {
		t.Fatal("unauthenticated local owner read paired device's job")
	}
	// Removing the paired credential revokes retrieval as well as new starts.
	if err = manager.Remove(peer.ID); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if _, err = source.AgentRemoteFetchArtifact(caller, peer.ID, receipt.ID, "report.txt"); err == nil {
		t.Fatal("removed pairing fetched artifact")
	}
}

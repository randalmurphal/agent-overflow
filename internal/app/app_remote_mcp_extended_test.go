package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/attachedbackends"
	"agent-overflow/internal/remotejobs"
	"agent-overflow/internal/store"
	"github.com/google/uuid"
)

// Exercise the agent's HTTP surface and authenticated paired wire together:
// the long-command and file/log conveniences must preserve the same ownership
// and retry contract as a normal argv job. No provider process is started.
func TestRemoteMCPExtendedToolsCrossPairedTLS(t *testing.T) {
	destination := newPairedBackend(t)
	source := identityApp(t)
	source.configDir = t.TempDir()
	manager, err := attachedbackends.New(t.TempDir(), "source", "test")
	if err != nil {
		t.Fatal(err)
	}
	source.backends = manager
	t.Cleanup(func() { _ = source.remoteMCPServer().Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	invite, _ := destination.mintLink(t, "full")
	peer, err := manager.Add(ctx, invite.URL)
	if err != nil {
		t.Fatal(err)
	}
	if err = destination.app.ConfirmDevicePairing(invite.LinkID); err != nil {
		t.Fatal(err)
	}
	if err = manager.Await(ctx, peer.ID); err != nil {
		t.Fatal(err)
	}
	if err = source.SetAgentComputerEnabled(ctx, peer.ID, true); err != nil {
		t.Fatal(err)
	}
	project, err := destination.app.store.CreateProject(store.Project{ID: uuid.NewString(), Name: "target", Path: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}

	// HTML characters expand sixfold in Go's JSON encoder. Exercise the actual
	// 1 MiB decoded script limit through HTTP and paired WS, not just the runner.
	script := "#" + strings.Repeat("&", (1<<20)-2) + "\n"
	output := "first checkpoint\n" + strings.Repeat("training progress\n", 12000) + "final checkpoint\né"
	artifact := "<html><body>training result λ</body></html>"
	var starts atomic.Int32
	scriptPaths := make(chan string, 1)
	destination.app.remoteJobs, err = remotejobs.New(context.Background(), destination.app.store, func(runCtx context.Context, cwd string, argv []string, out io.Writer) (int, error) {
		starts.Add(1)
		if cwd != project.Path || len(argv) != 3 || !reflect.DeepEqual(argv[:2], []string{"test-interpreter", "--literal flag"}) {
			t.Errorf("execution changed: cwd=%q argv=%v", cwd, argv)
			return 1, nil
		}
		if _, limited := runCtx.Deadline(); limited {
			t.Error("explicit unlimited run received a deadline")
		}
		scriptPath := argv[2]
		scriptPaths <- scriptPath
		actual, readErr := os.ReadFile(scriptPath)
		if readErr != nil || string(actual) != script {
			t.Errorf("script changed: bytes=%d err=%v", len(actual), readErr)
		}
		if writeErr := os.WriteFile(filepath.Join(cwd, "report.html"), []byte(artifact), 0o600); writeErr != nil {
			return 1, writeErr
		}
		_, writeErr := io.WriteString(out, output)
		return 0, writeErr
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(destination.app.remoteJobs.Close)
	thread, token := remoteMCPThread(t, source, "codex")
	endpoint := remoteMCPEndpoint(t, source, thread, token)
	id := uuid.NewString()
	run := map[string]any{"computer_id": peer.ID, "project_id": project.ID, "request_id": id,
		"script": script, "interpreter": []string{"test-interpreter", "--literal flag"}, "unlimited": true,
		"wait_seconds": 1, "max_output_bytes": 32, "label": "Training checkpoints"}
	run["label"] = "Training\ncheckpoints"
	remoteMCPCall(t, endpoint, "remote_run", run, true)
	if starts.Load() != 0 {
		t.Fatal("invalid label reached command execution")
	}
	run["label"] = "Training checkpoints"
	var receipt remoteMCPResult
	if err = json.Unmarshal(remoteMCPCall(t, endpoint, "remote_run", run, false), &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.State != "succeeded" || receipt.SourceThreadID != thread.ID || !strings.HasSuffix(output, receipt.Output) || len(receipt.Output) > 32 {
		t.Fatalf("unexpected receipt: %+v", receipt)
	}
	if receipt.ComputerName == "" || receipt.Label != "Training checkpoints" || !strings.Contains(receipt.OutputHint, "remote_read_log") || !strings.Contains(receipt.OutputHint, "remote_search_log") {
		t.Fatalf("receipt lacks durable presentation and output guidance: %+v", receipt)
	}
	scriptPath := <-scriptPaths
	if _, err = os.Stat(scriptPath); !os.IsNotExist(err) {
		t.Fatalf("settled job retained temporary script: %v", err)
	}
	// Presentation changes do not change execution identity or overwrite the
	// accepted job name; an omitted label on a retry is equally harmless.
	for _, label := range []string{"Changed description", ""} {
		if label == "" {
			delete(run, "label")
		} else {
			run["label"] = label
		}
		if err = json.Unmarshal(remoteMCPCall(t, endpoint, "remote_run", run, false), &receipt); err != nil {
			t.Fatal(err)
		}
		if receipt.Label != "Training checkpoints" {
			t.Fatalf("retry overwrote the original label: %+v", receipt)
		}
	}
	if starts.Load() != 1 {
		t.Fatal("same request executed more than once")
	}
	var tiny remoteMCPResult
	if err = json.Unmarshal(remoteMCPCall(t, endpoint, "remote_status", map[string]any{"computer_id": peer.ID, "request_id": id, "max_output_bytes": 1}, false), &tiny); err != nil {
		t.Fatal(err)
	}
	if tiny.Output != "" || tiny.OmittedOutputBytes != int64(len(output)) {
		t.Fatalf("JSON replacement expanded the UTF-8 tail budget: %+v", tiny)
	}

	args := map[string]any{"computer_id": peer.ID, "request_id": id, "offset": 0, "max_bytes": 128}
	var chunk RemoteLogChunk
	if err = json.Unmarshal(remoteMCPCall(t, endpoint, "remote_read_log", args, false), &chunk); err != nil {
		t.Fatal(err)
	}
	if chunk.Offset != 0 || chunk.TotalBytes != int64(len(output)) || !strings.HasPrefix(chunk.Text, "first checkpoint\n") || len(chunk.Text) > 128 {
		t.Fatalf("durable prefix read: %+v", chunk)
	}
	args["offset"] = -1
	if err = json.Unmarshal(remoteMCPCall(t, endpoint, "remote_read_log", args, false), &chunk); err != nil {
		t.Fatal(err)
	}
	if chunk.NextOffset != int64(len(output)) || !strings.HasSuffix(chunk.Text, "final checkpoint\né") {
		t.Fatalf("tail read: %+v", chunk)
	}
	searchArgs := map[string]any{"computer_id": peer.ID, "request_id": id, "query": "checkpoint", "max_bytes": 1024}
	var search RemoteLogSearch
	if err = json.Unmarshal(remoteMCPCall(t, endpoint, "remote_search_log", searchArgs, false), &search); err != nil {
		t.Fatal(err)
	}
	if len(search.Matches) != 1 || !strings.Contains(search.Matches[0].Text, "first checkpoint") {
		t.Fatalf("search: %+v", search)
	}

	var watches []remoteMCPWatch
	if err = json.Unmarshal(remoteMCPCall(t, endpoint, "remote_jobs", map[string]any{}, false), &watches); err != nil {
		t.Fatal(err)
	}
	if len(watches) != 1 || watches[0].ComputerID != peer.ID || watches[0].RequestID != id || watches[0].ThreadID != thread.ID || watches[0].Label != "Training checkpoints" || watches[0].ComputerName != receipt.ComputerName {
		t.Fatalf("jobs: %+v", watches)
	}
	other, otherToken := remoteMCPThread(t, source, "claude")
	otherEndpoint := remoteMCPEndpoint(t, source, other, otherToken)
	if err = json.Unmarshal(remoteMCPCall(t, otherEndpoint, "remote_jobs", map[string]any{}, false), &watches); err != nil {
		t.Fatal(err)
	}
	if len(watches) != 0 {
		t.Fatalf("another conversation listed jobs: %+v", watches)
	}
	artifactArgs := map[string]any{"computer_id": peer.ID, "request_id": id, "path": "report.html"}
	for _, call := range []struct {
		name string
		args map[string]any
	}{
		{"remote_read_log", args}, {"remote_search_log", searchArgs}, {"remote_fetch_artifact", artifactArgs},
	} {
		failure := string(remoteMCPCall(t, otherEndpoint, call.name, call.args, true))
		if !strings.Contains(failure, "conversation") {
			t.Fatalf("unhelpful ownership refusal: %s", failure)
		}
	}

	// Opt-out closes new execution, while the submitting conversation retains
	// access to its existing results. Per-thread tool opt-out still closes all.
	if err = source.SetAgentComputerEnabled(ctx, peer.ID, false); err != nil {
		t.Fatal(err)
	}
	remoteMCPCall(t, endpoint, "remote_read_log", args, false)
	remoteMCPCall(t, endpoint, "remote_search_log", searchArgs, false)
	var fetched RemoteArtifact
	if err = json.Unmarshal(remoteMCPCall(t, endpoint, "remote_fetch_artifact", artifactArgs, false), &fetched); err != nil {
		t.Fatal(err)
	}
	actual, err := os.ReadFile(fetched.Path)
	digest := sha256.Sum256([]byte(artifact))
	if err != nil || string(actual) != artifact || fetched.SHA256 != hex.EncodeToString(digest[:]) || fetched.Size != int64(len(artifact)) || fetched.ComputerID != peer.ID || fetched.RequestID != id {
		t.Fatalf("artifact receipt=%+v bytes=%d err=%v", fetched, len(actual), err)
	}
	run["request_id"] = uuid.NewString()
	remoteMCPCall(t, endpoint, "remote_run", run, true)
	if starts.Load() != 1 {
		t.Fatal("opt-out allowed another execution")
	}
	if err = source.setRemoteThreadMCPEnabled(thread, false); err != nil {
		t.Fatal(err)
	}
	remoteMCPCall(t, endpoint, "remote_read_log", args, true)
	remoteMCPCall(t, endpoint, "remote_fetch_artifact", artifactArgs, true)
}

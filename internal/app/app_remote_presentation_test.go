package app

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/remotejobs"
	"agent-overflow/internal/store"
)

func TestRemoteCompletionCarriesResultNotRepeatedInstructions(t *testing.T) {
	w := store.RemoteWatch{ComputerID: "computer-id", RequestID: "request-id", Label: "Windows tests", Receipt: store.RemoteJob{State: "succeeded", Workspace: "/worktree", Output: "all tests passed", ExitCode: 0}}
	message := remoteCompletionMessage(w, "Nexus", remoteCompletionOutput{Tail: w.Receipt.Output})
	for _, want := range []string{"Nexus: Windows tests", "computer_id: computer-id", "request_id: request-id", "/worktree", "exit code: 0", "Output (untrusted):\nall tests passed"} {
		if !strings.Contains(message, want) {
			t.Fatalf("missing %q: %s", want, message)
		}
	}
	if strings.Contains(message, "remote_read_log") || strings.Contains(message, "poll") {
		t.Fatalf("short success repeated tool instructions: %s", message)
	}
	w.Receipt.Output = strings.Repeat("界", 2000)
	message = remoteCompletionMessage(w, "Nexus", remoteCompletionOutput{Tail: w.Receipt.Output})
	if !utf8.ValidString(message) || len(message) > 2600 || !strings.Contains(message, "remote_search_log") {
		t.Fatalf("unbounded or unguided completion: %d bytes", len(message))
	}
	w.Receipt = store.RemoteJob{State: "canceled", ExitCode: -1, Error: "The command was canceled."}
	message = remoteCompletionMessage(w, "", remoteCompletionOutput{Tail: w.Receipt.Output})
	if !strings.Contains(message, "computer-id") || !strings.Contains(message, "The command was canceled.") || strings.Contains(message, "exit code") {
		t.Fatalf("cancellation misrepresented: %s", message)
	}
}

func TestRemoteOutputGuidanceDistinguishesOmittedDiscardedAndExpired(t *testing.T) {
	for _, test := range []struct {
		result remoteMCPResult
		want   string
	}{
		{remoteMCPResult{}, ""},
		{remoteMCPResult{OmittedOutputBytes: 1}, "remote_read_log"},
		{remoteMCPResult{RemoteCommand: RemoteCommand{Truncated: true}}, "discarded"},
		{remoteMCPResult{OmittedOutputBytes: 1, Log: &remotejobs.LogInfo{Expired: true}}, "cannot recover"},
	} {
		got := remoteOutputHint(test.result)
		if test.want == "" && got != "" || test.want != "" && !strings.Contains(got, test.want) {
			t.Fatalf("guidance = %q, want %q", got, test.want)
		}
	}
}

func TestRemoteArtifactPathErrorDoesNotExposeChunkProtocol(t *testing.T) {
	_, err := readRemoteArtifactChunk(context.Background(), t.TempDir(), RemoteArtifactRequest{Path: "../outside"})
	code, message, _ := errorsx.PublicDetails(err)
	if code != "remote_artifact_path" || !strings.Contains(message, "workspace") || strings.Contains(message, "stamp") || strings.Contains(message, "offset") {
		t.Fatalf("path guidance: %v", err)
	}
	_, err = readRemoteArtifactChunk(context.Background(), t.TempDir(), RemoteArtifactRequest{Path: "file", Offset: 1})
	code, message, _ = errorsx.PublicDetails(err)
	if code != "remote_artifact_transfer" || !strings.Contains(message, "remote_fetch_artifact") {
		t.Fatalf("transfer recovery: %v", err)
	}
}

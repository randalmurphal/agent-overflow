package sessionfork

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestTransferCopyScopesChildrenToNewRootAndKeepsMessageIdentity(t *testing.T) {
	projects, id, messageID := t.TempDir(), uuid.NewString(), uuid.NewString()
	childID := "a123456789abcdef"
	rootRow := map[string]any{"type": "user", "sessionId": id, "uuid": messageID, "parentUuid": nil, "cwd": "/source/workspace", "message": map[string]any{"role": "user", "content": "Remember " + id}}
	childRow := map[string]any{"type": "user", "sessionId": id, "uuid": messageID, "parentUuid": nil, "isSidechain": true, "agentId": childID, "message": map[string]any{"role": "user", "content": "saved child"}}
	files := map[string]any{"slug/" + id + ".jsonl": rootRow, "slug/" + id + "/subagents/agent-" + childID + ".jsonl": childRow, "slug/" + id + "/subagents/agent-" + childID + ".meta.json": map[string]any{"agentType": "general-purpose", "sessionId": id, "future": true}}
	for name, row := range files {
		file := filepath.Join(projects, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
			t.Fatal(err)
		}
		// Pretty metadata is supported; transcripts stay newline-delimited.
		data, err := json.Marshal(row)
		if strings.HasSuffix(name, ".meta.json") {
			data, err = json.MarshalIndent(row, "", "  ")
		}
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasSuffix(name, ".jsonl") {
			data = append(data, '\n')
		}
		if err := os.WriteFile(file, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	opaque := filepath.Join(projects, "slug", id, "future.bin")
	if err := os.WriteFile(opaque, []byte{0, 1, 255, 0}, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	sources, err := TransferFiles(ctx, projects, id, "")
	if err != nil {
		t.Fatal(err)
	}
	operation := uuid.NewString()
	copy, err := CopyTransferFiles(ctx, operation, id, filepath.Join(t.TempDir(), "copy"), sources)
	if err != nil {
		t.Fatal(err)
	}
	if copy.SessionID == id || len(copy.Files) != 4 {
		t.Fatalf("bad copy: %+v", copy)
	}
	for _, file := range copy.Files {
		if !strings.Contains(file.Path, copy.SessionID) || strings.Contains(file.Path, id) {
			t.Fatalf("old root path: %s", file.Path)
		}
		data, err := os.ReadFile(filepath.Join(file.Root, filepath.FromSlash(file.Path)))
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasSuffix(file.Path, ".bin") {
			if string(data) != string([]byte{0, 1, 255, 0}) {
				t.Fatal("opaque sidecar changed")
			}
			continue
		}
		var row map[string]any
		if err := json.Unmarshal(data, &row); err != nil {
			t.Fatal(err)
		}
		if row["sessionId"] != copy.SessionID {
			t.Fatalf("old root identity: %s", data)
		}
		if strings.HasSuffix(file.Path, ".jsonl") && row["uuid"] != messageID {
			t.Fatal("native message anchor changed")
		}
		if strings.Contains(file.Path, "/subagents/") && strings.HasSuffix(file.Path, ".jsonl") && (row["agentId"] != childID || row["isSidechain"] != true) {
			t.Fatal("child identity or chain changed")
		}
		if strings.HasSuffix(file.Path, copy.SessionID+".jsonl") && row["message"].(map[string]any)["content"] != "Remember "+id {
			t.Fatal("historical prose changed")
		}
	}
	again, err := CopyTransferFiles(ctx, operation, id, filepath.Join(t.TempDir(), "copy"), sources)
	if err != nil || again.SessionID != copy.SessionID {
		t.Fatalf("unstable copy identity: %+v %v", again, err)
	}
	// Both parent and child must be discoverable in the destination slug.
	copyProjects := filepath.Join(copy.Files[0].Root, "native", "claude")
	_, dest, err := RelocateSession(copyProjects, copy.SessionID, "", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dest), copy.SessionID, "subagents", "agent-"+childID+".jsonl")); err != nil {
		t.Fatalf("lost child during relocation: %v", err)
	}
	if _, err := os.Stat(filepath.Join(projects, "slug", id+".jsonl")); err != nil {
		t.Fatal("removed original")
	}
}

func TestTransferCopyRefusesPartialTranscriptWithoutPublishing(t *testing.T) {
	projects, id := t.TempDir(), uuid.NewString()
	directory := filepath.Join(projects, "slug")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(directory, id+".jsonl")
	if err := os.WriteFile(file, []byte(`{"sessionId":"`+id+`","type":"user"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	sources, err := TransferFiles(ctx, projects, id, "")
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "copy")
	if _, err := CopyTransferFiles(ctx, uuid.NewString(), id, destination, sources); err == nil {
		t.Fatal("accepted incomplete final record")
	}
	if _, err := os.Stat(destination); !os.IsNotExist(err) {
		t.Fatal("left failed copy directory")
	}
	if _, err := CopyTransferFiles(ctx, uuid.NewString(), id, projects, sources); err == nil {
		t.Fatal("accepted existing directory")
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatal("removed existing source after refusal")
	}
}

func TestTransferCopyPinnedPrefixNeverIncludesParentFuture(t *testing.T) {
	ctx := context.Background()
	projects, id := t.TempDir(), uuid.NewString()
	directory := filepath.Join(projects, "slug")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	content := `{"type":"user","uuid":"kept","sessionId":"` + id + `","message":{"role":"user","content":"kept history"}}` + "\n" +
		`{"type":"assistant","uuid":"pin","sessionId":"` + id + `","parentUuid":"kept","message":{"role":"assistant","content":[{"type":"text","text":"saved answer"}]}}` + "\n" +
		`{"type":"user","uuid":"future","sessionId":"` + id + `","parentUuid":"pin","message":{"role":"user","content":"parent future"}}` + "\n"
	if err := os.WriteFile(filepath.Join(directory, id+".jsonl"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	sources, err := TransferFiles(ctx, projects, id, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, pin := range []string{"pin", "missing"} {
		destination := filepath.Join(t.TempDir(), "fork")
		copy, err := CopyTransferFilesAt(ctx, TransferCopyCut{OperationID: uuid.NewString(), SessionID: id, Destination: destination, ThroughUUID: pin}, sources)
		if pin == "missing" {
			if err == nil {
				t.Fatal("missing pin became a full-history fork")
			}
			if _, err := os.Stat(destination); !os.IsNotExist(err) {
				t.Fatal("left failed prefix", err)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		file := copy.Files[0]
		data, err := os.ReadFile(filepath.Join(file.Root, filepath.FromSlash(file.Path)))
		if err != nil || !strings.Contains(string(data), "saved answer") || strings.Contains(string(data), "parent future") || strings.Count(string(data), "\n") != 2 {
			t.Fatalf("wrong prefix: %s %v", data, err)
		}
	}
}

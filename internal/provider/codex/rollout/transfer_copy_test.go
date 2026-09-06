package rollout

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/transferfiles"
	"github.com/google/uuid"
)

func nativeTransferFixture(t *testing.T, home, id string, rows ...map[string]any) TransferReference {
	t.Helper()
	file := filepath.Join(home, "sessions", "2026", "09", "05", "rollout-2026-09-05T12-00-00-"+id+".jsonl")
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	var data []byte
	for _, row := range rows {
		encoded, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		data = append(data, encoded...)
		data = append(data, '\n')
	}
	if err := os.WriteFile(file, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return TransferReference{SessionID: id, Path: file}
}

func nativeTransferRow(kind string, payload map[string]any) map[string]any {
	return map[string]any{"timestamp": "2026-09-05T12:00:00Z", "type": kind, "payload": payload}
}

func nativeTransferMeta(id, root, parent string) map[string]any {
	p := map[string]any{"id": id, "session_id": root, "cwd": "/old/checkout", "history_mode": "legacy"}
	if parent != "" {
		p["parent_thread_id"] = parent
		p["source"] = map[string]any{"subagent": map[string]any{"thread_spawn": map[string]any{"parent_thread_id": parent, "depth": 1}}}
	}
	return nativeTransferRow("session_meta", p)
}

func TestTransferGraphCopyKeepsChildrenIndependentAndProseUnchanged(t *testing.T) {
	ctx, home := context.Background(), t.TempDir()
	rootID, childID, grandchildID, unrelated := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	spawn := func(id string) map[string]any {
		return nativeTransferRow("response_item", map[string]any{"type": "function_call", "call_id": "spawn-" + id, "name": "spawn_agent", "arguments": `{"prompt":"historical ` + id + `"}`})
	}
	result := func(id string) map[string]any {
		return nativeTransferRow("response_item", map[string]any{"type": "function_call_output", "call_id": "spawn-" + id, "output": `{"agent_id":"` + id + `","text":"historical ` + id + `"}`})
	}
	root := nativeTransferFixture(t, home, rootID, nativeTransferMeta(rootID, rootID, ""), spawn(childID), result(childID),
		nativeTransferRow("response_item", map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": unrelated}}}),
		nativeTransferRow("response_item", map[string]any{"type": "function_call", "call_id": "shell", "name": "exec_command", "arguments": `{"id":"` + unrelated + `"}`}),
		nativeTransferRow("response_item", map[string]any{"type": "function_call_output", "call_id": "shell", "output": `{"agent_id":"` + unrelated + `"}`}))
	child := nativeTransferFixture(t, home, childID, nativeTransferMeta(childID, rootID, rootID), spawn(grandchildID), result(grandchildID))
	grandchild := nativeTransferFixture(t, home, grandchildID, nativeTransferMeta(grandchildID, rootID, childID))
	byID := map[string]TransferReference{childID: child, grandchildID: grandchild}
	resolved := make(map[string]int)
	refs, sources, err := TransferGraph(ctx, home, root, func(_ context.Context, id string) (TransferReference, error) {
		resolved[id]++
		ref, ok := byID[id]
		if !ok {
			return ref, fmt.Errorf("unexpected dependency %s", id)
		}
		return ref, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 3 || len(sources) != 3 || resolved[childID] != 1 || resolved[grandchildID] != 1 {
		t.Fatalf("closure: %+v %+v", refs, resolved)
	}
	before, _ := os.ReadFile(root.Path)
	operation := uuid.NewString()
	copy, err := CopyTransferFiles(ctx, operation, filepath.Join(t.TempDir(), "copy"), sources)
	if err != nil {
		t.Fatal(err)
	}
	if len(copy.IDs) != 3 || copy.IDs[rootID] == rootID || copy.IDs[childID] == childID {
		t.Fatalf("identities: %+v", copy.IDs)
	}
	copyAgain, err := CopyTransferFiles(ctx, operation, filepath.Join(t.TempDir(), "copy"), sources)
	if err != nil || copyAgain.IDs[childID] != copy.IDs[childID] {
		t.Fatalf("unstable retry identity: %+v %v", copyAgain.IDs, err)
	}
	data, err := os.ReadFile(filepath.Join(copy.Files[0].Root, filepath.FromSlash(copy.Files[0].Path)))
	if err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatal(err)
		}
		rows = append(rows, row)
	}
	output := rows[2]["payload"].(map[string]any)["output"].(string)
	var decoded map[string]any
	_ = json.Unmarshal([]byte(output), &decoded)
	if decoded["agent_id"] != copy.IDs[childID] || decoded["text"] != "historical "+childID {
		t.Fatalf("output rewrite changed prose: %s", output)
	}
	if !strings.Contains(string(data), unrelated) || copy.IDs[unrelated] != "" {
		t.Fatal("ordinary tool output or message became an execution dependency")
	}
	childMeta, err := readTransferMeta(ctx, copy.Files[1], copy.IDs[childID])
	if err != nil || childMeta.ParentThreadID != copy.IDs[rootID] {
		t.Fatalf("copied child parent: %+v %v", childMeta, err)
	}
	after, _ := os.ReadFile(root.Path)
	if string(before) != string(after) {
		t.Fatal("modified original")
	}
}

func TestTransferCopyRepairsPrefixByteCoordinatesAndRetainsOrdinals(t *testing.T) {
	ctx, home := context.Background(), t.TempDir()
	baseID, leafID, nativeID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	base := nativeTransferFixture(t, home, baseID, nativeTransferMeta(nativeID, nativeID, ""),
		nativeTransferRow("response_item", map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "original history"}}}))
	// Whitespace makes original and transformed metadata byte lengths differ.
	data, _ := os.ReadFile(base.Path)
	data = append([]byte("  "), data...)
	if err := os.WriteFile(base.Path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	meta := nativeTransferMeta(nativeID, nativeID, "")
	payload := meta["payload"].(map[string]any)
	payload["history_mode"] = "paginated"
	payload["history_base"] = map[string]any{"thread_id": baseID, "end_ordinal_exclusive": 7, "end_byte_offset": len(data)}
	leaf := nativeTransferFixture(t, home, leafID, meta)
	leaf.SessionID = nativeID
	sources, err := TransferFiles(ctx, home, []TransferReference{leaf})
	if err != nil {
		t.Fatal(err)
	}
	copy, err := CopyTransferFiles(ctx, uuid.NewString(), filepath.Join(t.TempDir(), "copy"), sources)
	if err != nil {
		t.Fatal(err)
	}
	copiedMeta, err := readTransferMeta(ctx, copy.Files[1], copy.IDs[nativeID])
	if err != nil {
		t.Fatal(err)
	}
	copiedBase, err := os.Stat(filepath.Join(copy.Files[0].Root, filepath.FromSlash(copy.Files[0].Path)))
	if err != nil {
		t.Fatal(err)
	}
	if copiedMeta.HistoryBase.ThreadID != copy.IDs[baseID] || copiedMeta.HistoryBase.EndOrdinalExclusive != 7 || copiedMeta.HistoryBase.EndByteOffset != uint64(copiedBase.Size()) || copiedBase.Size() == int64(len(data)) {
		t.Fatalf("bad prefix coordinates: %+v source=%d dest=%d", copiedMeta.HistoryBase, len(data), copiedBase.Size())
	}
	if _, err := CopyTransferFiles(ctx, uuid.NewString(), filepath.Join(t.TempDir(), "copy"), []transferfiles.Source{sources[1], sources[0]}); err == nil {
		t.Fatal("accepted unordered prefix")
	}
	payload["history_base"].(map[string]any)["end_byte_offset"] = len(data) - 1
	nativeTransferFixture(t, home, leafID, meta)
	dest := filepath.Join(t.TempDir(), "copy")
	if _, err := CopyTransferFiles(ctx, uuid.NewString(), dest, sources); err == nil {
		t.Fatal("accepted position inside a native record")
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatal("failed copy left a publishable directory")
	}
}

func TestTransferCopyRetainsSeparateConversationAndRolloutFilenameIDs(t *testing.T) {
	ctx, home := context.Background(), t.TempDir()
	native, segment, nextSegment := uuid.NewString(), uuid.NewString(), uuid.NewString()
	base := nativeTransferFixture(t, home, native, nativeTransferMeta(native, native, ""))
	baseData, err := os.ReadFile(base.Path)
	if err != nil {
		t.Fatal(err)
	}
	makeSegment := func(fileID, baseID string, offset int) TransferReference {
		meta := nativeTransferMeta(native, native, "")
		payload := meta["payload"].(map[string]any)
		payload["history_mode"] = "paginated"
		payload["history_base"] = map[string]any{"thread_id": baseID, "end_ordinal_exclusive": 1, "end_byte_offset": offset}
		ref := nativeTransferFixture(t, home, fileID, meta)
		newPath := strings.TrimSuffix(ref.Path, fileID+".jsonl") + native + "_" + fileID + ".jsonl"
		if err := os.Rename(ref.Path, newPath); err != nil {
			t.Fatal(err)
		}
		return TransferReference{SessionID: native, Path: newPath}
	}
	middle := makeSegment(segment, native, len(baseData))
	middleData, err := os.ReadFile(middle.Path)
	if err != nil {
		t.Fatal(err)
	}
	leaf := makeSegment(nextSegment, segment, len(middleData))
	sources, err := TransferFiles(ctx, home, []TransferReference{leaf})
	if err != nil || len(sources) != 3 {
		t.Fatalf("reverted prefix closure: %+v %v", sources, err)
	}
	copy, err := CopyTransferFiles(ctx, uuid.NewString(), filepath.Join(t.TempDir(), "copy"), sources)
	if err != nil {
		t.Fatal(err)
	}
	for i, fileID := range []string{native, segment, nextSegment} {
		file := copy.Files[i]
		session, rollout := rolloutFileIDs(file.Name)
		if session != copy.IDs[native] || rollout != copy.IDs[fileID] || strings.Contains(file.Name, native) {
			t.Fatalf("wrong copied filename identities: %s", file.Name)
		}
		meta, err := readTransferMeta(ctx, file, "")
		if err != nil || meta.SessionID != copy.IDs[native] {
			t.Fatalf("filename picked wrong native meta: %+v %v", meta, err)
		}
	}
	last := copy.Files[2]
	destinationHome := filepath.Join(last.Root, "native", "codex")
	again, err := TransferFiles(ctx, destinationHome, []TransferReference{{SessionID: copy.IDs[native], Path: filepath.Join(last.Root, filepath.FromSlash(last.Path))}})
	if err != nil || len(again) != 3 {
		t.Fatalf("copied closure cannot be moved again: %+v %v", again, err)
	}
}

func TestTransferCollaborationContentItemsPreserveOpaqueData(t *testing.T) {
	child, newChild := uuid.NewString(), uuid.NewString()
	records := transferRecords{identity: func(id string) string {
		if id == child {
			return newChild
		}
		return id
	}}
	call := `{"type":"response_item","payload":{"type":"function_call","name":"spawn_agent","call_id":"spawn","arguments":"{\"large\":9007199254740993}"}}`
	mapped, err := records.rewrite([]byte(call))
	if err != nil || !strings.Contains(string(mapped), "9007199254740993") {
		t.Fatal("changed argument integer", string(mapped), err)
	}
	text := `{"agent_id":"` + child + `","large":9007199254740993,"prose":"` + child + `"}`
	body, _ := json.Marshal(nativeTransferRow("response_item", map[string]any{"type": "function_call_output", "call_id": "spawn", "output": []any{
		map[string]any{"type": "input_text", "text": text}, map[string]any{"type": "input_image", "image_url": "https://example.invalid/" + child}, map[string]any{"type": "encrypted_content", "encrypted_content": child},
	}}))
	mapped, err = records.rewrite(body)
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Payload struct {
			Output []map[string]any `json:"output"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(mapped, &envelope); err != nil {
		t.Fatal(err)
	}
	result, ok := transferJSONObject(envelope.Payload.Output[0]["text"].(string))
	if !ok || result["agent_id"] != newChild || result["prose"] != child || result["large"] != json.Number("9007199254740993") {
		t.Fatalf("lost structured identity or opaque data: %s", mapped)
	}
	if envelope.Payload.Output[1]["image_url"] != "https://example.invalid/"+child || envelope.Payload.Output[2]["encrypted_content"] != child {
		t.Fatal("rewrote opaque content")
	}
}

func TestTransferMetaUsesForkHeaderInsteadOfInheritedHeader(t *testing.T) {
	home, ctx := t.TempDir(), context.Background()
	forkID, sourceID := uuid.NewString(), uuid.NewString()
	ref := nativeTransferFixture(t, home, forkID, nativeTransferMeta(forkID, forkID, ""), nativeTransferMeta(sourceID, sourceID, ""))
	files, err := TransferFiles(ctx, home, []TransferReference{ref})
	if err != nil {
		t.Fatal(err)
	}
	meta, err := readTransferMeta(ctx, files[0], "")
	if err != nil || meta.SessionID != forkID {
		t.Fatalf("wrong authoritative header: %+v %v", meta, err)
	}
}

func TestTransferCopyRefusesIncompleteRecordAndExistingDirectory(t *testing.T) {
	home, ctx := t.TempDir(), context.Background()
	id := uuid.NewString()
	ref := nativeTransferFixture(t, home, id, nativeTransferMeta(id, id, ""))
	files, err := TransferFiles(ctx, home, []TransferReference{ref})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := CopyTransferFiles(ctx, uuid.NewString(), t.TempDir(), files); err == nil {
		t.Fatal("accepted existing destination")
	}
	f, err := os.OpenFile(ref.Path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"type":"response_item"`)
	_ = f.Close()
	if _, err := CopyTransferFiles(ctx, uuid.NewString(), filepath.Join(t.TempDir(), "copy"), files); err == nil {
		t.Fatal("silently dropped partial native history")
	}
}

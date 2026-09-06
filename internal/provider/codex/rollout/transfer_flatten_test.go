package rollout

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func paginatedTransferFixture(t *testing.T) (string, TransferReference, string) {
	t.Helper()
	home, native, segment, nextSegment, futureChild := t.TempDir(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	meta := nativeTransferMeta(native, native, "")
	meta["payload"].(map[string]any)["history_mode"] = "paginated"
	rows := []map[string]any{meta,
		nativeTransferRow("response_item", map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "retained first turn"}}}),
		nativeTransferRow("response_item", map[string]any{"type": "function_call", "name": "spawn_agent", "call_id": "future-call", "arguments": "{}"}),
		nativeTransferRow("response_item", map[string]any{"type": "function_call_output", "call_id": "future-call", "output": `{"agent_id":"` + futureChild + `"}`}),
	}
	var prefixBytes int
	for i, row := range rows {
		row["ordinal"] = i
		encoded, _ := json.Marshal(row)
		if i < 2 {
			prefixBytes += len(encoded) + 1
		}
	}
	nativeTransferFixture(t, home, native, rows...)
	makeSegment := func(fileID, base string, ordinal, offset int, body bool) TransferReference {
		header := nativeTransferMeta(native, native, "")
		header["ordinal"] = ordinal
		payload := header["payload"].(map[string]any)
		payload["history_mode"] = "paginated"
		payload["history_base"] = map[string]any{"thread_id": base, "end_ordinal_exclusive": ordinal, "end_byte_offset": offset}
		rows := []map[string]any{header}
		if body {
			row := nativeTransferRow("event_msg", map[string]any{"type": "task_complete", "turn_id": "preserved-turn-id", "last_agent_message": "retained later answer"})
			row["ordinal"] = ordinal + 1
			rows = append(rows, row)
		}
		ref := nativeTransferFixture(t, home, fileID, rows...)
		newPath := strings.TrimSuffix(ref.Path, fileID+".jsonl") + native + "_" + fileID + ".jsonl"
		if err := os.Rename(ref.Path, newPath); err != nil {
			t.Fatal(err)
		}
		return TransferReference{SessionID: native, Path: newPath}
	}
	middle := makeSegment(segment, native, 2, prefixBytes, true)
	middleData, err := os.ReadFile(middle.Path)
	if err != nil {
		t.Fatal(err)
	}
	leaf := makeSegment(nextSegment, segment, 4, len(middleData), false)
	return home, leaf, futureChild
}

func TestTransferMaterializesOnlyRetainedPaginatedLineage(t *testing.T) {
	ctx := context.Background()
	home, leaf, future := paginatedTransferFixture(t)
	refs, sources, err := TransferGraph(ctx, home, leaf, func(_ context.Context, id string) (TransferReference, error) {
		return TransferReference{}, fmt.Errorf("discarded future child was requested: %s (future %s)", id, future)
	})
	if err != nil || len(refs) != 1 || len(sources) != 3 {
		t.Fatalf("retained graph: %+v %v", refs, err)
	}
	before, err := os.ReadFile(leaf.Path)
	if err != nil {
		t.Fatal(err)
	}
	name := sources[2].Name
	flattened, err := FlattenTransferFiles(ctx, filepath.Join(t.TempDir(), "flattened"), sources, map[string]string{leaf.SessionID: name})
	if err != nil || len(flattened) != 1 {
		t.Fatalf("flatten: %+v %v", flattened, err)
	}
	file := flattened[0]
	data, err := os.ReadFile(filepath.Join(file.Root, filepath.FromSlash(file.Path)))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), future) || strings.Contains(string(data), "history_base") || !strings.Contains(string(data), "retained first turn") || !strings.Contains(string(data), "preserved-turn-id") {
		t.Fatalf("wrong retained history: %s", data)
	}
	for i, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var row struct {
			Ordinal int `json:"ordinal"`
		}
		if err := json.Unmarshal([]byte(line), &row); err != nil || row.Ordinal != i {
			t.Fatalf("nonsequential native ordinal: %s", line)
		}
	}
	meta, err := readTransferMeta(ctx, file, leaf.SessionID)
	if err != nil || meta.HistoryBase != nil || meta.HistoryMode != HistoryModePaginated {
		t.Fatalf("not standalone native history: %+v %v", meta, err)
	}
	after, err := os.ReadFile(leaf.Path)
	if err != nil || string(before) != string(after) {
		t.Fatal("materialization modified native source", err)
	}
	copy, err := CopyTransferFiles(ctx, uuid.NewString(), filepath.Join(t.TempDir(), "copy"), flattened)
	if err != nil || copy.IDs[leaf.SessionID] == leaf.SessionID {
		t.Fatal("standalone history could not fork", err)
	}
}

func TestTransferMaterializationRefusesInconsistentPrefixCoordinates(t *testing.T) {
	for _, key := range []string{"end_byte_offset", "end_ordinal_exclusive"} {
		t.Run(key, func(t *testing.T) {
			home, leaf, _ := paginatedTransferFixture(t)
			data, err := os.ReadFile(leaf.Path)
			if err != nil {
				t.Fatal(err)
			}
			var row map[string]any
			if err := json.Unmarshal(data, &row); err != nil {
				t.Fatal(err)
			}
			base := row["payload"].(map[string]any)["history_base"].(map[string]any)
			base[key] = base[key].(float64) - 1
			data, err = json.Marshal(row)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(leaf.Path, append(data, '\n'), 0o600); err != nil {
				t.Fatal(err)
			}
			sources, err := TransferFiles(context.Background(), home, []TransferReference{leaf})
			if err != nil {
				t.Fatal(err)
			}
			destination := filepath.Join(t.TempDir(), "flattened")
			if _, err := FlattenTransferFiles(context.Background(), destination, sources, map[string]string{leaf.SessionID: sources[len(sources)-1].Name}); err == nil {
				t.Fatal("rounded corrupt prefix boundary")
			}
			if _, err := os.Stat(destination); !os.IsNotExist(err) {
				t.Fatal("left partial materialization", err)
			}
		})
	}
}

func TestTransferMaterializationPreservesSubagentProjectionBoundary(t *testing.T) {
	for _, tc := range []struct {
		original, want uint64
		invalid        bool
	}{
		{0, 0, false}, {2, 2, false}, {3, 2, false}, {4, 3, false}, {5, 3, false}, {6, 0, true},
	} {
		t.Run(fmt.Sprint(tc.original), func(t *testing.T) {
			home, leaf, _ := paginatedTransferFixture(t)
			data, err := os.ReadFile(leaf.Path)
			if err != nil {
				t.Fatal(err)
			}
			var header map[string]any
			if err := json.Unmarshal(data, &header); err != nil {
				t.Fatal(err)
			}
			payload := header["payload"].(map[string]any)
			payload["subagent_history_start_ordinal"] = tc.original
			// This is a coordinate on the logical parent's history, not ours.
			payload["forked_from_ordinal_exclusive"] = 99
			data, err = json.Marshal(header)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(leaf.Path, append(data, '\n'), 0o600); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			sources, err := TransferFiles(ctx, home, []TransferReference{leaf})
			if err != nil {
				t.Fatal(err)
			}
			flat, err := FlattenTransferFiles(ctx, filepath.Join(t.TempDir(), "flat"), sources, map[string]string{leaf.SessionID: sources[len(sources)-1].Name})
			if tc.invalid {
				if err == nil {
					t.Fatal("accepted incomplete inherited child history")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			data, err = os.ReadFile(filepath.Join(flat[0].Root, filepath.FromSlash(flat[0].Path)))
			if err != nil {
				t.Fatal(err)
			}
			var result struct {
				Payload struct {
					Start  uint64 `json:"subagent_history_start_ordinal"`
					Parent uint64 `json:"forked_from_ordinal_exclusive"`
				} `json:"payload"`
			}
			if err := json.Unmarshal([]byte(strings.SplitN(string(data), "\n", 2)[0]), &result); err != nil {
				t.Fatal(err)
			}
			if result.Payload.Start != tc.want || result.Payload.Parent != 99 {
				t.Fatalf("wrong history coordinates: %+v, want start %d, parent 99", result, tc.want)
			}
		})
	}
}

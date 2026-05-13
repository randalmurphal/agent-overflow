package checkpoint

import (
	"reflect"
	"testing"

	"agent-overflow/internal/diffsummary"
	"agent-overflow/internal/store"
)

func TestViewFromStoreCopiesWireFields(t *testing.T) {
	row := store.Checkpoint{
		ID:                    "cp-1",
		ThreadID:              "thread-1",
		UserItemID:            "user:7",
		TurnIndex:             7,
		ProviderUserMessageID: "msg-abc",
		ProviderParentUUID:    "parent-xyz",
		RefName:               "refs/agent-overflow/checkpoints/x/y",
		BaselineSHA:           "sha-abc",
		Status:                "ready",
		Files:                 []diffsummary.File{{Path: "a.txt"}},
		CapturedAt:            1234,
		WorkspacePath:         "/workspace",
	}
	got := ViewFromStore(row)
	want := View{
		ID:                    "cp-1",
		ThreadID:              "thread-1",
		UserItemID:            "user:7",
		TurnIndex:             7,
		ProviderUserMessageID: "msg-abc",
		Status:                "ready",
		Files:                 []diffsummary.File{{Path: "a.txt"}},
		CapturedAt:            1234,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ViewFromStore = %+v, want %+v", got, want)
	}
}

package projectapp

import (
	"slices"
	"testing"

	"agent-overflow/internal/store"
)

func TestThreadLockOrderParentsBeforeChildren(t *testing.T) {
	threads := map[string]store.Thread{
		"z-parent":     {ID: "z-parent"},
		"a-child":      {ID: "a-child", ParentThreadID: "z-parent"},
		"m-sibling":    {ID: "m-sibling", ParentThreadID: "z-parent"},
		"b-grandchild": {ID: "b-grandchild", ParentThreadID: "a-child"},
	}
	want := []string{"z-parent", "a-child", "b-grandchild", "m-sibling"}
	if got := ThreadLockOrder(threads); !slices.Equal(got, want) {
		t.Fatalf("ThreadLockOrder = %v, want %v", got, want)
	}
}

func TestThreadLockOrderDoesNotDropCorruptCycle(t *testing.T) {
	threads := map[string]store.Thread{
		"a": {ID: "a", ParentThreadID: "b"},
		"b": {ID: "b", ParentThreadID: "a"},
	}
	if got := ThreadLockOrder(threads); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("ThreadLockOrder cycle = %v, want every id deterministically", got)
	}
}

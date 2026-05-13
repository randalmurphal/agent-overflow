package checkpoint

import (
	"testing"

	"agent-overflow/internal/store"
)

func TestIsWireOnlyUserItemTrue(t *testing.T) {
	item := store.Item{Meta: `{"wire_only": true, "provider_item_id": "msg-1"}`}
	if !IsWireOnlyUserItem(item) {
		t.Fatal("IsWireOnlyUserItem(wire_only=true) = false, want true")
	}
}

func TestIsWireOnlyUserItemFalse(t *testing.T) {
	item := store.Item{Meta: `{"wire_only": false}`}
	if IsWireOnlyUserItem(item) {
		t.Fatal("IsWireOnlyUserItem(wire_only=false) = true, want false")
	}
}

func TestIsWireOnlyUserItemMissingKey(t *testing.T) {
	item := store.Item{Meta: `{"provider_item_id": "msg-1"}`}
	if IsWireOnlyUserItem(item) {
		t.Fatal("IsWireOnlyUserItem(no key) = true, want false")
	}
}

func TestIsWireOnlyUserItemEmptyMeta(t *testing.T) {
	if IsWireOnlyUserItem(store.Item{Meta: ""}) {
		t.Fatal("IsWireOnlyUserItem(empty meta) = true, want false")
	}
}

func TestIsWireOnlyUserItemInvalidJSON(t *testing.T) {
	if IsWireOnlyUserItem(store.Item{Meta: "not json"}) {
		t.Fatal("IsWireOnlyUserItem(invalid json) = true, want false (safe default)")
	}
}

func TestIsWireOnlyUserItemWrongType(t *testing.T) {
	// A meta where `wire_only` is present but not a bool. Type
	// assertion fails silently → false (safe default).
	item := store.Item{Meta: `{"wire_only": "yes"}`}
	if IsWireOnlyUserItem(item) {
		t.Fatal("IsWireOnlyUserItem(wire_only=\"yes\") = true, want false")
	}
}

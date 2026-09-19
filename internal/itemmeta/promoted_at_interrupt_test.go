package itemmeta

import (
	"encoding/json"
	"testing"
)

func mustDecodePromotion(t *testing.T, raw string) PromotionState {
	t.Helper()
	state, err := DecodePromotionState(raw)
	if err != nil {
		t.Fatalf("DecodePromotionState(%q): %v", raw, err)
	}
	return state
}

func TestMarkPromotedAtInterruptEmptyMeta(t *testing.T) {
	got, err := MarkPromotedAtInterrupt("")
	if err != nil {
		t.Fatalf("MarkPromotedAtInterrupt(\"\"): %v", err)
	}
	if state := mustDecodePromotion(t, got); !state.Promoted || state.HasEchoBoundary {
		t.Errorf("marked empty meta %q decoded to %+v, want promoted without boundary", got, state)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(got), &m); err != nil {
		t.Fatalf("marked meta is not valid JSON: %v", err)
	}
	if len(m) != 1 {
		t.Errorf("marked empty meta = %q, want single-key object", got)
	}
}

func TestMarkPromotedAtInterruptPreservesOtherKeys(t *testing.T) {
	got, err := MarkPromotedAtInterrupt(`{"provider_item_id":"u-1","foo":42}`)
	if err != nil {
		t.Fatalf("MarkPromotedAtInterrupt: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(got), &m); err != nil {
		t.Fatalf("unmarshal marked meta: %v", err)
	}
	if m["provider_item_id"] != "u-1" || m["foo"] != float64(42) {
		t.Errorf("existing keys lost: %q", got)
	}
	if state := mustDecodePromotion(t, got); !state.Promoted {
		t.Errorf("marked meta %q should decode as promoted", got)
	}
}

func TestMarkPromotedAtInterruptIdempotent(t *testing.T) {
	once, err := MarkPromotedAtInterrupt(`{"foo":"bar"}`)
	if err != nil {
		t.Fatalf("first mark: %v", err)
	}
	twice, err := MarkPromotedAtInterrupt(once)
	if err != nil {
		t.Fatalf("second mark: %v", err)
	}
	if twice != once {
		t.Errorf("second mark rewrote the meta: %q -> %q", once, twice)
	}
}

func TestMarkPromotedAtInterruptRejectsMalformed(t *testing.T) {
	if _, err := MarkPromotedAtInterrupt(`{not json`); err == nil {
		t.Fatal("expected error for malformed meta")
	}
}

func TestMarkPromotedEchoBoundaryRoundTrips(t *testing.T) {
	marked, err := MarkPromotedAtInterrupt(`{"provider_item_id":"u-1"}`)
	if err != nil {
		t.Fatalf("mark promoted: %v", err)
	}
	stamped, err := MarkPromotedEchoBoundary(marked, 7)
	if err != nil {
		t.Fatalf("mark boundary: %v", err)
	}
	state := mustDecodePromotion(t, stamped)
	if !state.Promoted || !state.HasEchoBoundary || state.EchoBoundary != 7 {
		t.Errorf("decoded state = %+v, want promoted with boundary 7", state)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(stamped), &m); err != nil {
		t.Fatalf("unmarshal stamped meta: %v", err)
	}
	if m["provider_item_id"] != "u-1" {
		t.Errorf("provider_item_id lost through boundary stamp: %q", stamped)
	}
}

func TestMarkPromotedEchoBoundaryRejectsMalformed(t *testing.T) {
	if _, err := MarkPromotedEchoBoundary(`{not json`, 3); err == nil {
		t.Fatal("expected error for malformed meta")
	}
}

func TestDecodePromotionStateUnmarkedReadings(t *testing.T) {
	for _, raw := range []string{
		"",
		`{"foo":"bar"}`,
		`{"promoted_at_interrupt":false}`,
	} {
		if state := mustDecodePromotion(t, raw); state.Promoted || state.HasEchoBoundary {
			t.Errorf("DecodePromotionState(%q) = %+v, want zero state", raw, state)
		}
	}
}

// Wrong-typed values are corrupt data, not absent markers (round-9,
// R9-4): silently reading them as absent flips revert/fork onto the
// wrong cut predicate, desynchronizing SQLite from the provider slice.
func TestDecodePromotionStateRejectsMalformed(t *testing.T) {
	for _, raw := range []string{
		`{not json`,
		// A literal JSON null unmarshals into a nil map without error —
		// non-empty meta that decodes to no object is corrupt, not an
		// unpromoted anchor (round-14, C14-4).
		`null`,
		`{"promoted_at_interrupt":"true"}`,
		`{"promoted_at_interrupt":1}`,
		`{"promoted_at_interrupt":true,"promoted_echo_boundary":"5"}`,
		`{"promoted_at_interrupt":true,"promoted_echo_boundary":5.5}`,
		`{"promoted_at_interrupt":true,"promoted_echo_boundary":-3}`,
		`{"promoted_echo_boundary":null}`,
		// A boundary without (or with a false) promotion marker is an
		// impossible persisted state — no writer produces it and the
		// marker is never cleared (round-13, C13-3).
		`{"promoted_echo_boundary":5}`,
		`{"promoted_at_interrupt":false,"promoted_echo_boundary":5}`,
	} {
		if _, err := DecodePromotionState(raw); err == nil {
			t.Errorf("DecodePromotionState(%q) = nil error, want rejection — truncation callers must not silently degrade", raw)
		}
	}
}

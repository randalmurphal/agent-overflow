package eventscope

import (
	"encoding/json"
	"testing"
)

// rowLike and itemFrameLike emulate store.Item and triage.ItemStreamEvent
// for the reflection branch without importing either package. The real
// shapes are pinned against this extractor in internal/triage
// (TestItemStreamEventsCarryTheirScopeRoot).
type rowLike struct {
	ID       string
	ThreadID string
	ParentID string
}

type itemFrameLike struct {
	Action   string
	ThreadID string
	Item     *rowLike
	ItemID   string
	ParentID string
}

func TestScopeRootIDFromTopLevelParent(t *testing.T) {
	evt := itemFrameLike{Action: "delta", ThreadID: "t", ItemID: "child", ParentID: "anchor"}
	if got := ScopeRootIDFromEvent(evt); got != "anchor" {
		t.Errorf("got %q, want anchor", got)
	}
}

func TestScopeRootIDFromCarriedRow(t *testing.T) {
	evt := itemFrameLike{Action: "upsert", ThreadID: "t", Item: &rowLike{ID: "child", ParentID: "anchor"}}
	if got := ScopeRootIDFromEvent(evt); got != "anchor" {
		t.Errorf("got %q, want anchor", got)
	}
	if got := ScopeRootIDFromEvent(&evt); got != "anchor" {
		t.Errorf("pointer payload: got %q, want anchor", got)
	}
}

func TestScopeRootIDTopLevelWins(t *testing.T) {
	evt := itemFrameLike{ParentID: "outer", Item: &rowLike{ParentID: "inner"}}
	if got := ScopeRootIDFromEvent(evt); got != "outer" {
		t.Errorf("got %q, want outer", got)
	}
}

func TestScopeRootIDRootRowsAreEmpty(t *testing.T) {
	for name, evt := range map[string]itemFrameLike{
		"rootUpsert": {Action: "upsert", Item: &rowLike{ID: "top"}},
		"rootDelta":  {Action: "delta", ItemID: "top"},
		"nilRow":     {Action: "remove", ItemID: "top"},
	} {
		if got := ScopeRootIDFromEvent(evt); got != "" {
			t.Errorf("%s: got %q, want the root scope", name, got)
		}
	}
}

// countingFrame is a frame shape the reflection branch recognises, with a
// MarshalJSON that records whether the fallback ran.
type countingFrame struct {
	ParentID string
	Item     *rowLike
	marshals *int
}

func (f countingFrame) MarshalJSON() ([]byte, error) {
	*f.marshals++
	return []byte(`{}`), nil
}

// TestScopeRootIDRecognisedShapesSkipTheJSONFallback pins the hot-path
// property: every provider:item_event is derived here, most of them root
// scope, and a root answer must not cost a marshal round trip.
func TestScopeRootIDRecognisedShapesSkipTheJSONFallback(t *testing.T) {
	marshals := 0
	for _, evt := range []countingFrame{
		{marshals: &marshals},
		{Item: &rowLike{}, marshals: &marshals},
		{Item: &rowLike{ParentID: "anchor"}, marshals: &marshals},
	} {
		ScopeRootIDFromEvent(evt)
	}
	if marshals != 0 {
		t.Fatalf("a recognised frame shape took the JSON fallback %d times", marshals)
	}
}

func TestScopeRootIDFromMaps(t *testing.T) {
	if got := ScopeRootIDFromEvent(map[string]any{"parentId": "anchor"}); got != "anchor" {
		t.Errorf("top-level map: got %q", got)
	}
	if got := ScopeRootIDFromEvent(map[string]any{"item": map[string]any{"parentId": "anchor"}}); got != "anchor" {
		t.Errorf("nested map: got %q", got)
	}
	if got := ScopeRootIDFromEvent(map[string]string{"parentId": " anchor "}); got != "anchor" {
		t.Errorf("string map: got %q", got)
	}
}

// TestScopeRootIDFromRawPayloads covers the harness's raw frames, which
// reach the funnel as json.RawMessage.
func TestScopeRootIDFromRawPayloads(t *testing.T) {
	for raw, want := range map[string]string{
		`{"action":"delta","threadId":"t","itemId":"c","parentId":"anchor"}`:        "anchor",
		`{"action":"upsert","threadId":"t","item":{"id":"c","parentId":"anchor"}}`:  "anchor",
		`{"action":"upsert","threadId":"t","item":{"id":"top"}}`:                    "",
		`{"action":"patch","threadId":"t","itemId":"c","parentId":"  spaced  "}`:    "spaced",
		`{"action":"upsert","threadId":"t","item":{"id":"c","parentId":["wrong"]}}`: "",
		`[1,2,3]`: "",
	} {
		if got := ScopeRootIDFromEvent(json.RawMessage(raw)); got != want {
			t.Errorf("%s: got %q, want %q", raw, got, want)
		}
	}
}

func TestScopeRootIDAbsent(t *testing.T) {
	if got := ScopeRootIDFromEvent(nil); got != "" {
		t.Errorf("nil: got %q", got)
	}
	if got := ScopeRootIDFromEvent("bare string"); got != "" {
		t.Errorf("string: got %q", got)
	}
	var nilFrame *itemFrameLike
	if got := ScopeRootIDFromEvent(nilFrame); got != "" {
		t.Errorf("nil pointer: got %q", got)
	}
	if got := ScopeRootIDFromEvent(map[string]any{"threadId": "t"}); got != "" {
		t.Errorf("thread-only map: got %q", got)
	}
}

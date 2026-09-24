package eventscope

import (
	"encoding/json"
	"reflect"
	"strings"
)

// ScopeRootIDFromEvent does a best-effort lookup of the transcript scope a
// timeline-row payload belongs to: the `parentId` of the row it is about.
// An empty result means the root scope (a top-level row) or a payload this
// helper cannot attribute; callers treat both as root scope, which is the
// delivering direction (internal/transport/event_entity.go).
//
// A frame names its row's parent in one of two places: at the top level
// (`parentId`, on frames that carry no row) or inside the row it carries
// (`item.parentId`). The top-level value wins when both are present.
//
// Lookup order:
//  1. map[string]any / map[string]string with a "parentId" key, or a
//     map[string]any "item" holding one.
//  2. Struct (or pointer-to-struct) with an exported ParentID string field
//     or an exported Item struct (or pointer-to-struct) field carrying
//     one. A struct that declares either field is answered here, empty
//     included, so a root-scope frame never pays the JSON round trip.
//  3. JSON round-trip fallback for every other shape, including the raw
//     payloads the harness publishes.
//
// The id is space-trimmed before return, like ThreadIDFromEvent's.
func ScopeRootIDFromEvent(data any) string {
	if data == nil {
		return ""
	}
	switch payload := data.(type) {
	case map[string]any:
		return scopeRootFromMap(payload)
	case map[string]string:
		return strings.TrimSpace(payload["parentId"])
	case string:
		return ""
	}
	v := reflect.ValueOf(data)
	for v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return ""
		}
		v = v.Elem()
	}
	if v.Kind() == reflect.Struct {
		if id, declared := scopeRootFromStruct(v); declared {
			return id
		}
	}
	encoded, err := json.Marshal(data)
	if err != nil {
		return ""
	}
	var generic struct {
		ParentID string `json:"parentId"`
		Item     *struct {
			ParentID string `json:"parentId"`
		} `json:"item"`
	}
	if err := json.Unmarshal(encoded, &generic); err != nil {
		return ""
	}
	if id := strings.TrimSpace(generic.ParentID); id != "" {
		return id
	}
	if generic.Item != nil {
		return strings.TrimSpace(generic.Item.ParentID)
	}
	return ""
}

func scopeRootFromMap(payload map[string]any) string {
	if id, ok := payload["parentId"].(string); ok {
		if trimmed := strings.TrimSpace(id); trimmed != "" {
			return trimmed
		}
	}
	if item, ok := payload["item"].(map[string]any); ok {
		if id, ok := item["parentId"].(string); ok {
			return strings.TrimSpace(id)
		}
	}
	return ""
}

// scopeRootFromStruct reads the two fields by reflection. declared reports
// whether the struct has either field at all; only a struct with neither
// falls through to the JSON fallback.
func scopeRootFromStruct(v reflect.Value) (id string, declared bool) {
	if f := v.FieldByName("ParentID"); f.IsValid() && f.Kind() == reflect.String {
		declared = true
		if trimmed := strings.TrimSpace(f.String()); trimmed != "" {
			return trimmed, true
		}
	}
	item := v.FieldByName("Item")
	if !item.IsValid() {
		return "", declared
	}
	for item.Kind() == reflect.Pointer {
		if item.IsNil() {
			return "", true
		}
		item = item.Elem()
	}
	if item.Kind() != reflect.Struct {
		return "", declared
	}
	if f := item.FieldByName("ParentID"); f.IsValid() && f.Kind() == reflect.String {
		return strings.TrimSpace(f.String()), true
	}
	return "", declared
}

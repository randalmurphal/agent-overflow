package eventscope

import (
	"encoding/json"
	"reflect"
	"strings"
)

// ThreadIDFromEvent does a best-effort lookup of the thread id on an
// event payload. The transport's Emit takes `any`, so the payload can
// be almost anything; this helper handles the common shapes rather than
// trying to enumerate every event struct in the codebase.
//
// Lookup order:
//  1. map[string]any / map[string]string with a "threadId" key (the
//     most common shape for frontend-visible events).
//  2. Struct (or pointer-to-struct) with an exported ThreadID field —
//     the shape on provider.ProviderEvent.
//  3. JSON round-trip fallback for anonymous struct literals that embed
//     a `threadId` tag without exposing a direct Go field name we can
//     reach via reflection.
//
// Returns the empty string if no thread id is found. The id is always
// space-trimmed before return so callers can use string-equality
// against thread-row PKs without re-trimming.
func ThreadIDFromEvent(data any) string {
	if data == nil {
		return ""
	}

	switch payload := data.(type) {
	case map[string]any:
		if id, ok := payload["threadId"].(string); ok {
			return strings.TrimSpace(id)
		}
	case map[string]string:
		return strings.TrimSpace(payload["threadId"])
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
		if f := v.FieldByName("ThreadID"); f.IsValid() && f.Kind() == reflect.String {
			return strings.TrimSpace(f.String())
		}
	}

	encoded, err := json.Marshal(data)
	if err != nil {
		return ""
	}
	var generic map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &generic); err != nil {
		return ""
	}
	raw, ok := generic["threadId"]
	if !ok {
		return ""
	}
	var id string
	if err := json.Unmarshal(raw, &id); err != nil {
		return ""
	}
	return strings.TrimSpace(id)
}

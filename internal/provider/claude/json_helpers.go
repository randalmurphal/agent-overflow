package claude

import "encoding/json"

// readIntAtAnyKey returns the first integer-valued field in `data` matching
// one of `keys`. Works on both plain objects and objects nested inside an
// array (returning the first hit).
//
// Only an object or an array can hold one, so any other value, such as a
// tool result's output text, is answered from its first byte rather than
// decoded.
func readIntAtAnyKey(data json.RawMessage, keys ...string) (int, bool) {
	start := 0
	for start < len(data) && (data[start] == ' ' || data[start] == '\t' || data[start] == '\n' || data[start] == '\r') {
		start++
	}
	if start == len(data) || (data[start] != '{' && data[start] != '[') {
		return 0, false
	}

	var obj map[string]json.RawMessage
	if json.Unmarshal(data, &obj) == nil {
		for _, key := range keys {
			if v, ok := obj[key]; ok {
				var n int
				if json.Unmarshal(v, &n) == nil {
					return n, true
				}
			}
		}
		return 0, false
	}

	var arr []map[string]json.RawMessage
	if json.Unmarshal(data, &arr) == nil {
		for _, entry := range arr {
			for _, key := range keys {
				if v, ok := entry[key]; ok {
					var n int
					if json.Unmarshal(v, &n) == nil {
						return n, true
					}
				}
			}
		}
	}
	return 0, false
}

func readRawString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var value string
	if json.Unmarshal(raw, &value) == nil {
		return value
	}
	return ""
}

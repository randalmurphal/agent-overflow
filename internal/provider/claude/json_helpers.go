package claude

import "encoding/json"

// readIntAtAnyKey returns the first integer-valued field in `data` matching
// one of `keys`. Works on both plain objects and objects nested inside an
// array (returning the first hit).
func readIntAtAnyKey(data json.RawMessage, keys ...string) (int, bool) {
	if len(data) == 0 {
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

// readStringAtAnyKey returns the first string-valued field in `data`
// matching one of `keys`. The string sibling of readIntAtAnyKey: works on
// both plain objects and objects nested inside an array (first hit wins).
// Returns ("", false) when absent, empty raw, or non-string.
func readStringAtAnyKey(data json.RawMessage, keys ...string) (string, bool) {
	if len(data) == 0 {
		return "", false
	}

	var obj map[string]json.RawMessage
	if json.Unmarshal(data, &obj) == nil {
		for _, key := range keys {
			if v, ok := obj[key]; ok {
				var s string
				if json.Unmarshal(v, &s) == nil {
					return s, true
				}
			}
		}
		return "", false
	}

	var arr []map[string]json.RawMessage
	if json.Unmarshal(data, &arr) == nil {
		for _, entry := range arr {
			for _, key := range keys {
				if v, ok := entry[key]; ok {
					var s string
					if json.Unmarshal(v, &s) == nil {
						return s, true
					}
				}
			}
		}
	}
	return "", false
}

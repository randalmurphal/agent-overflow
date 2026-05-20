package claudeconfig

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// orderedJSON is a JSON object with stable key order. Values are kept
// as raw bytes unless the caller mutates them, in which case the
// caller swaps the entry for a typed value (or another *orderedJSON).
// On marshal, untouched entries are emitted byte-identical to the
// input — only the mutated paths actually re-encode.
//
// This is the smallest amount of order-preserving JSON we need. The
// ~/.claude.json file has 50+ top-level keys and 200+ project entries;
// dropping into a `map[string]any` and re-marshaling would resort all
// of them alphabetically every time AO touched the file.
type orderedJSON struct {
	keys   []string
	values map[string]any
}

func newOrderedJSON() *orderedJSON {
	return &orderedJSON{values: map[string]any{}}
}

func parseOrderedJSON(data []byte) (*orderedJSON, error) {
	o := newOrderedJSON()
	if len(bytes.TrimSpace(data)) == 0 {
		return o, nil
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	tk, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("read opening token: %w", err)
	}
	if d, ok := tk.(json.Delim); !ok || d != '{' {
		return nil, fmt.Errorf("expected JSON object, got %v", tk)
	}
	for dec.More() {
		ktk, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("read key token: %w", err)
		}
		key, ok := ktk.(string)
		if !ok {
			return nil, fmt.Errorf("expected string key, got %T", ktk)
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, fmt.Errorf("read value for %q: %w", key, err)
		}
		o.keys = append(o.keys, key)
		o.values[key] = raw
	}
	return o, nil
}

func (o *orderedJSON) has(key string) bool {
	_, ok := o.values[key]
	return ok
}

func (o *orderedJSON) get(key string) (any, bool) {
	v, ok := o.values[key]
	return v, ok
}

// getObject returns the value at key as an *orderedJSON, parsing the
// raw bytes once and caching the result by replacing the map entry.
// Subsequent calls return the same *orderedJSON so mutations stick.
func (o *orderedJSON) getObject(key string) (*orderedJSON, error) {
	v, ok := o.values[key]
	if !ok {
		return nil, nil
	}
	if obj, ok := v.(*orderedJSON); ok {
		return obj, nil
	}
	raw, ok := v.(json.RawMessage)
	if !ok {
		return nil, fmt.Errorf("value at %q is not a raw object: %T", key, v)
	}
	obj, err := parseOrderedJSON(raw)
	if err != nil {
		return nil, fmt.Errorf("parse %q as object: %w", key, err)
	}
	o.values[key] = obj
	return obj, nil
}

// set replaces or appends an entry. Appended keys go to the end so
// untouched entries keep their original position.
func (o *orderedJSON) set(key string, v any) {
	if _, exists := o.values[key]; !exists {
		o.keys = append(o.keys, key)
	}
	o.values[key] = v
}

// delete removes a key entirely.
func (o *orderedJSON) delete(key string) {
	if _, exists := o.values[key]; !exists {
		return
	}
	delete(o.values, key)
	for i, k := range o.keys {
		if k == key {
			o.keys = append(o.keys[:i], o.keys[i+1:]...)
			return
		}
	}
}

// MarshalJSON emits compact JSON in stable key order. Nested
// *orderedJSON values are recursively encoded; raw bytes pass through
// unchanged. Use json.Indent on the result to get the pretty form.
func (o *orderedJSON) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, k := range o.keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		kb, err := json.Marshal(k)
		if err != nil {
			return nil, err
		}
		buf.Write(kb)
		buf.WriteByte(':')
		switch v := o.values[k].(type) {
		case json.RawMessage:
			buf.Write(v)
		case *orderedJSON:
			vb, err := v.MarshalJSON()
			if err != nil {
				return nil, err
			}
			buf.Write(vb)
		default:
			vb, err := json.Marshal(v)
			if err != nil {
				return nil, err
			}
			buf.Write(vb)
		}
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// marshalIndented produces the pretty form used when writing the file
// back to disk. Indent is two spaces to match Claude Code's own
// formatting (no trailing newline at EOF, matching the observed file).
func (o *orderedJSON) marshalIndented() ([]byte, error) {
	compact, err := o.MarshalJSON()
	if err != nil {
		return nil, err
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, compact, "", "  "); err != nil {
		return nil, err
	}
	return pretty.Bytes(), nil
}

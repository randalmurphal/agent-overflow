package adf

import (
	"cmp"
	"encoding/json"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Doc wraps content in a version 1 doc node.
func Doc(content ...*Node) *Node {
	return &Node{Type: "doc", Version: 1, Content: content}
}

func textNode(s string, marks ...Mark) *Node {
	n := &Node{Type: "text", Text: s}
	if len(marks) > 0 {
		n.Marks = append([]Mark(nil), marks...)
	}
	return n
}

func (n *Node) attr(key string) any {
	if n == nil || n.Attrs == nil {
		return nil
	}
	return n.Attrs[key]
}

func (n *Node) attrStr(key string) string {
	s, _ := n.attr(key).(string)
	return s
}

func (n *Node) attrNum(key string) (float64, bool) {
	switch v := n.attr(key).(type) {
	case float64:
		return v, true
	case int:
		return float64(v), true
	case json.Number:
		f, err := v.Float64()
		return f, err == nil
	}
	return 0, false
}

func (n *Node) setAttr(key string, v any) {
	if n.Attrs == nil {
		n.Attrs = map[string]any{}
	}
	n.Attrs[key] = v
}

func hasMark(n *Node, typ string) bool {
	for _, m := range n.Marks {
		if m.Type == typ {
			return true
		}
	}
	return false
}

// markRank orders marks the way the Atlassian editor schema does, so
// output is stable.
var markRank = lazy[map[string]int]{build: func() map[string]int {
	return map[string]int{
		"link": 0, "em": 1, "strong": 2, "textColor": 3, "strike": 4,
		"subsup": 5, "underline": 6, "code": 7, "backgroundColor": 8,
		"annotation": 9,
	}
}}

func sortMarks(ms []Mark) {
	rank := func(m Mark) int {
		if r, ok := markRank.get()[m.Type]; ok {
			return r
		}
		return len(markRank.get())
	}
	slices.SortStableFunc(ms, func(a, b Mark) int {
		return cmp.Compare(rank(a), rank(b))
	})
}

func markEqual(a, b Mark) bool {
	if a.Type != b.Type || len(a.Attrs) != len(b.Attrs) {
		return false
	}
	for k, v := range a.Attrs {
		if !valueEqual(v, b.Attrs[k]) {
			return false
		}
	}
	return true
}

func marksEqual(a, b []Mark) bool {
	if len(a) != len(b) {
		return false
	}
	for _, m := range a {
		found := false
		for _, o := range b {
			if markEqual(m, o) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// valueEqual reports whether a and b encode to the same JSON, which is
// how attribute values compare (an int 2 equals a float64 2). Scalars of
// one type compare directly; anything else is encoded.
func valueEqual(a, b any) bool {
	switch x := a.(type) {
	case nil:
		if b == nil {
			return true
		}
	case string:
		if y, ok := b.(string); ok && utf8.ValidString(x) &&
			utf8.ValidString(y) {
			return x == y
		}
	case bool:
		if y, ok := b.(bool); ok {
			return x == y
		}
	case float64:
		if y, ok := b.(float64); ok && !math.IsInf(x, 0) {
			return x == y && math.Signbit(x) == math.Signbit(y)
		}
	}
	ja, err1 := json.Marshal(a)
	jb, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && string(ja) == string(jb)
}

// mergeText joins adjacent text nodes that carry the same marks and
// drops empty ones. It returns a new slice and never modifies the nodes.
func mergeText(ns []*Node) []*Node {
	out := make([]*Node, 0, len(ns))
	for i := 0; i < len(ns); i++ {
		n := ns[i]
		if n.Type == "text" && n.Text == "" {
			continue
		}
		if n.Type != "text" {
			out = append(out, n)
			continue
		}
		var joined strings.Builder
		run := 0
		for j := i + 1; j < len(ns); j++ {
			m := ns[j]
			if m.Type == "text" && m.Text == "" {
				i = j
				continue
			}
			if m.Type != "text" || !marksEqual(n.Marks, m.Marks) {
				break
			}
			if run == 0 {
				joined.WriteString(n.Text)
			}
			joined.WriteString(m.Text)
			run++
			i = j
		}
		if run > 0 {
			cp := *n
			cp.Text = joined.String()
			n = &cp
		}
		out = append(out, n)
	}
	return out
}

// key is a canonical encoding of n that ignores localId attributes, used
// to compare nodes during Edit alignment. Two keys are equal exactly when
// the nodes' JSON is, apart from localIds and the order of marks.
func key(n *Node) string {
	var b strings.Builder
	writeKey(&b, n)
	return b.String()
}

func writeKey(b *strings.Builder, n *Node) {
	b.WriteString("{")
	b.WriteString(n.Type)
	if n.Text != "" {
		b.WriteString("|t:")
		writeKeyString(b, n.Text)
	}
	if len(n.Attrs) > 0 {
		for _, k := range sortedKeys(n.Attrs) {
			if k != "localId" {
				b.WriteString("|" + k + "=")
				writeKeyValue(b, n.Attrs[k])
			}
		}
	}
	if len(n.Marks) > 0 {
		ms := make([]string, len(n.Marks))
		for i, m := range n.Marks {
			var mb strings.Builder
			mb.WriteString(m.Type)
			writeKeyValue(&mb, m.Attrs)
			ms[i] = mb.String()
		}
		slices.Sort(ms)
		b.WriteString("|m:" + strings.Join(ms, ","))
	}
	for _, c := range n.Content {
		writeKey(b, c)
	}
	b.WriteString("}")
}

func sortedKeys(m map[string]any) []string {
	return slices.Sorted(maps.Keys(m))
}

// writeKeyValue writes v so that two values write the same exactly when
// encoding/json encodes them the same: ints and whole floats alike, -0
// apart from 0. Types JSON values don't decode to are JSON-encoded.
func writeKeyValue(b *strings.Builder, v any) {
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
		return
	case string:
		writeKeyString(b, x)
		return
	case bool:
		b.WriteString(strconv.FormatBool(x))
		return
	case int:
		b.WriteString(strconv.Itoa(x))
		return
	case float64:
		if x == math.Trunc(x) && math.Abs(x) < 1e15 &&
			(x != 0 || !math.Signbit(x)) {
			b.WriteString(strconv.FormatInt(int64(x), 10))
			return
		}
	case []any:
		if x == nil {
			b.WriteString("null")
			return
		}
		b.WriteString("[")
		for i, e := range x {
			if i > 0 {
				b.WriteString(",")
			}
			writeKeyValue(b, e)
		}
		b.WriteString("]")
		return
	case map[string]any:
		if x == nil {
			b.WriteString("null")
			return
		}
		b.WriteString("{")
		for i, k := range sortedKeys(x) {
			if i > 0 {
				b.WriteString(",")
			}
			writeKeyString(b, k)
			b.WriteString(":")
			writeKeyValue(b, x[k])
		}
		b.WriteString("}")
		return
	}
	js, err := json.Marshal(v)
	if err != nil {
		b.WriteString("!" + err.Error()) // unencodable: never equal to JSON
		return
	}
	b.Write(js)
}

// writeKeyString quotes s. Like encoding/json, it reads each invalid
// UTF-8 byte as U+FFFD.
func writeKeyString(b *strings.Builder, s string) {
	b.WriteByte('"')
	if utf8.ValidString(s) && !strings.ContainsAny(s, `"\\`) {
		b.WriteString(s)
		b.WriteByte('"')
		return
	}
	for _, r := range s {
		switch r {
		case '"', '\\':
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	b.WriteByte('"')
}

// cloneNode deep-copies n.
func cloneNode(n *Node) *Node {
	if n == nil {
		return nil
	}
	c := *n
	if n.Attrs != nil {
		c.Attrs = cloneValue(n.Attrs).(map[string]any)
	}
	if n.Marks != nil {
		c.Marks = make([]Mark, len(n.Marks))
		for i, m := range n.Marks {
			c.Marks[i] = Mark{Type: m.Type}
			if m.Attrs != nil {
				c.Marks[i].Attrs = cloneValue(m.Attrs).(map[string]any)
			}
		}
	}
	if n.Content != nil {
		c.Content = make([]*Node, len(n.Content))
		for i, ch := range n.Content {
			c.Content[i] = cloneNode(ch)
		}
	}
	return &c
}

func cloneValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(t))
		for k, x := range t {
			m[k] = cloneValue(x)
		}
		return m
	case []any:
		s := make([]any, len(t))
		for i, x := range t {
			s[i] = cloneValue(x)
		}
		return s
	}
	return v
}

// dropNil returns n without the nil nodes JSON like "content":[null]
// decodes to, copying only the nodes on a path to one.
func dropNil(n *Node) *Node {
	if n == nil {
		return nil
	}
	var kept []*Node
	changed := false
	for i, c := range n.Content {
		d := dropNil(c)
		if (c == nil || d != c) && !changed {
			changed = true
			kept = append(make([]*Node, 0, len(n.Content)), n.Content[:i]...)
		}
		if changed && d != nil {
			kept = append(kept, d)
		}
	}
	if !changed {
		return n
	}
	cp := *n
	cp.Content = kept
	return &cp
}

// walk calls fn for n and every descendant, parents first.
func walk(n *Node, fn func(n, parent *Node)) {
	var rec func(n, p *Node)
	rec = func(n, p *Node) {
		fn(n, p)
		for _, c := range n.Content {
			rec(c, n)
		}
	}
	rec(n, nil)
}

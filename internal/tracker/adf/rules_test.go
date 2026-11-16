package adf

import (
	"fmt"
	"runtime"
	"sort"
	"sync"
	"testing"
)

// minimal builds the smallest valid node of each type.
func minimal(t string) *Node {
	p := func() *Node { return &Node{Type: "paragraph"} }
	a := func(kv ...any) map[string]any {
		m := map[string]any{}
		for i := 0; i < len(kv); i += 2 {
			m[kv[i].(string)] = kv[i+1]
		}
		return m
	}
	n := &Node{Type: t}
	switch t {
	case "text":
		n.Text = "x"
	case "heading":
		n.Attrs = a("level", 1)
	case "bulletList", "orderedList":
		n.Content = []*Node{minimal("listItem")}
	case "listItem", "blockquote", "tableCell", "tableHeader", "expand":
		n.Content = []*Node{p()}
	case "panel":
		n.Attrs, n.Content = a("panelType", "info"), []*Node{p()}
	case "table":
		n.Content = []*Node{minimal("tableRow")}
	case "tableRow":
		n.Content = []*Node{minimal("tableCell")}
	case "nestedExpand":
		n.Attrs, n.Content = a("title", ""), []*Node{p()}
	case "extension", "inlineExtension":
		n.Attrs = a("extensionKey", "k", "extensionType", "t")
	case "bodiedExtension":
		n.Attrs = a("extensionKey", "k", "extensionType", "t")
		n.Content = []*Node{p()}
	case "layoutSection":
		n.Content = []*Node{minimal("layoutColumn"), minimal("layoutColumn")}
	case "layoutColumn":
		n.Attrs, n.Content = a("width", 50), []*Node{p()}
	case "taskList":
		n.Attrs, n.Content = a("localId", "l"), []*Node{minimal("taskItem")}
	case "taskItem", "decisionItem":
		n.Attrs = a("localId", "i", "state", "TODO")
	case "blockTaskItem":
		n.Attrs, n.Content = a("localId", "i", "state", "TODO"), []*Node{p()}
	case "decisionList":
		n.Attrs = a("localId", "l")
		n.Content = []*Node{minimal("decisionItem")}
	case "mediaSingle":
		n.Attrs, n.Content = a("layout", "center"), []*Node{minimal("media")}
	case "mediaGroup":
		n.Content = []*Node{minimal("media")}
	case "media":
		n.Attrs = a("type", "file", "id", "m", "collection", "")
	case "mediaInline":
		n.Attrs = a("id", "m", "collection", "")
	case "blockCard", "inlineCard":
		n.Attrs = a("url", "https://example.com")
	case "embedCard":
		n.Attrs = a("url", "https://example.com", "layout", "center")
	case "mention":
		n.Attrs = a("id", "u")
	case "emoji":
		n.Attrs = a("shortName", ":x:")
	case "date":
		n.Attrs = a("timestamp", "1")
	case "status":
		n.Attrs = a("text", "S", "color", "red")
	case "placeholder":
		n.Attrs = a("text", "p")
	case "syncBlock":
		n.Attrs = a("resourceId", "r", "localId", "l")
	case "bodiedSyncBlock":
		n.Attrs, n.Content = a("resourceId", "r", "localId", "l"), []*Node{p()}
	}
	return n
}

// place puts child inside a node of type parent and wraps that in the
// ancestors it needs to be valid.
func place(parent string, child *Node) *Node {
	n := minimal(parent)
	n.Content = []*Node{child}
	wrap := func(outer string, inner *Node) *Node {
		o := minimal(outer)
		o.Content = []*Node{inner}
		return o
	}
	switch parent {
	case "doc":
		return Doc(child)
	case "listItem":
		return Doc(wrap("bulletList", n))
	case "tableCell", "tableHeader":
		return Doc(wrap("table", wrap("tableRow", n)))
	case "tableRow":
		return Doc(wrap("table", n))
	case "taskItem":
		return Doc(wrap("taskList", n))
	case "blockTaskItem":
		return Doc(wrap("taskList", n))
	case "decisionItem":
		return Doc(wrap("decisionList", n))
	case "nestedExpand":
		return Doc(wrap("expand", n))
	case "layoutColumn":
		s := minimal("layoutSection")
		s.Content[0] = n
		return Doc(s)
	case "caption":
		ms := minimal("mediaSingle")
		ms.Content = append(ms.Content, n)
		return Doc(ms)
	case "media":
		return Doc(wrap("mediaGroup", n))
	}
	return Doc(n)
}

type ruleCase struct {
	name string
	doc  *Node
}

func checkAgainstSchema(t *testing.T, cases []ruleCase,
	deviations map[string]bool) {
	t.Helper()
	type result struct {
		name         string
		ours, theirs bool
	}
	results := make([]result, len(cases))
	var wg sync.WaitGroup
	next := make(chan int)
	for w := 0; w < runtime.GOMAXPROCS(0); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				c := cases[i]
				results[i] = result{c.name, Validate(c.doc) == nil,
					schemaCheck(t, c.doc) == nil}
			}
		}()
	}
	for i := range cases {
		next <- i
	}
	close(next)
	wg.Wait()
	var bad []string
	for _, r := range results {
		if r.ours != r.theirs && !deviations[r.name] {
			bad = append(bad, fmt.Sprintf("%s: Validate ok=%v, schema ok=%v",
				r.name, r.ours, r.theirs))
		}
	}
	sort.Strings(bad)
	for _, b := range bad {
		t.Error(b)
	}
}

// TestRulesMatchSchema places every node type under every container type
// and checks that Validate and the published schema agree.
func TestRulesMatchSchema(t *testing.T) {
	var types []string
	for k := range specs {
		if k != "doc" {
			types = append(types, k)
		}
	}
	sort.Strings(types)
	var cases []ruleCase
	for _, parent := range append([]string{"doc"}, types...) {
		if specs[parent].content == nil {
			continue
		}
		for _, child := range types {
			cases = append(cases, ruleCase{parent + ">" + child,
				place(parent, minimal(child))})
		}
	}
	checkAgainstSchema(t, cases, map[string]bool{
		// The schema allows these only inside synced blocks, which this
		// package does not build; Validate is stricter.
		"bodiedSyncBlock>layoutSection": true,
		// Confluence stores one-column layouts; Validate accepts them.
		"layoutSection>layoutColumn": true,
	})
}

// TestMarksMatchSchema checks mark rules on text and on block nodes in
// the contexts where the schema varies them.
func TestMarksMatchSchema(t *testing.T) {
	marks := map[string]Mark{
		"strong": {Type: "strong"}, "em": {Type: "em"},
		"code": {Type: "code"}, "strike": {Type: "strike"},
		"underline": {Type: "underline"},
		"link":      {Type: "link", Attrs: map[string]any{"href": "u"}},
		"subsup":    {Type: "subsup", Attrs: map[string]any{"type": "sub"}},
		"textColor": {Type: "textColor",
			Attrs: map[string]any{"color": "#aabbcc"}},
		"backgroundColor": {Type: "backgroundColor",
			Attrs: map[string]any{"color": "#aabbcc"}},
		"alignment": {Type: "alignment",
			Attrs: map[string]any{"align": "center"}},
		"indentation": {Type: "indentation",
			Attrs: map[string]any{"level": 1}},
		"breakout": {Type: "breakout",
			Attrs: map[string]any{"mode": "wide"}},
		"fontSize": {Type: "fontSize",
			Attrs: map[string]any{"fontSize": "small"}},
	}
	var names []string
	for k := range marks {
		names = append(names, k)
	}
	sort.Strings(names)
	var cases []ruleCase
	for _, a := range names {
		for _, b := range names {
			if a > b {
				continue
			}
			txt := minimal("text")
			txt.Marks = []Mark{marks[a]}
			if a != b {
				txt.Marks = append(txt.Marks, marks[b])
			}
			para := minimal("paragraph")
			para.Content = []*Node{txt}
			cases = append(cases, ruleCase{"text:" + a + "+" + b, Doc(para)})
		}
	}
	for _, parent := range []string{"doc", "listItem", "tableCell",
		"blockquote", "panel", "expand", "layoutColumn", "nestedExpand"} {
		for _, child := range []string{"paragraph", "heading", "codeBlock",
			"expand", "rule", "panel"} {
			if !allowed(parent, child) {
				continue
			}
			for _, m := range names {
				n := minimal(child)
				n.Marks = []Mark{marks[m]}
				cases = append(cases, ruleCase{
					parent + ">" + child + "/" + m, place(parent, n)})
			}
		}
	}
	checkAgainstSchema(t, cases, map[string]bool{
		// Confluence writes breakout on top-level rules and panels.
		"doc>rule/breakout": true, "doc>panel/breakout": true,
	})
}

package adf

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"unicode"
)

// canon normalizes nodes for semantic comparison: it drops localIds, the
// marks Markdown can't show, defaults, and whitespace that Markdown
// can't place (at line edges, or emphasized).
func canon(ns []*Node) []*Node {
	var out []*Node
	for _, n := range ns {
		n = cloneNode(n)
		delete(n.Attrs, "localId")
		var ms []Mark
		for _, m := range n.Marks {
			if _, lossyMark := markPhrases.get()[m.Type]; !lossyMark {
				ms = append(ms, m)
			}
		}
		sortMarks(ms)
		n.Marks = ms
		switch n.Type {
		case "mention":
			delete(n.Attrs, "accessLevel")
			delete(n.Attrs, "userType")
		case "orderedList":
			if f, ok := n.attrNum("order"); !ok || f == 1 {
				n.setAttr("order", 1.0)
			} else {
				n.setAttr("order", f)
			}
		case "codeBlock":
			if n.attrStr("language") == "" {
				delete(n.Attrs, "language")
			}
		case "hardBreak":
			n.Attrs = nil
		case "tableCell", "tableHeader":
			for _, k := range []string{"colspan", "rowspan"} {
				if f, ok := n.attrNum(k); ok && f == 1 {
					delete(n.Attrs, k)
				}
			}
		case "heading":
			if f, ok := n.attrNum("level"); ok {
				n.setAttr("level", f)
			}
		}
		if len(n.Attrs) == 0 {
			n.Attrs = nil
		}
		n.Content = canon(n.Content)
		if isInlineParent(n.Type) && n.Type != "codeBlock" {
			n.Content = canonInline(n.Content)
		}
		if len(n.Content) == 0 {
			n.Content = nil
		}
		out = append(out, n)
	}
	return out
}

func canonInline(ns []*Node) []*Node {
	var split []*Node
	for _, n := range ns {
		if n.Type != "text" {
			split = append(split, n)
			continue
		}
		for _, part := range spaceRuns(n.Text) {
			p := textNode(part, n.Marks...)
			if strings.TrimSpace(part) == "" {
				p.Marks = nil
				for _, m := range n.Marks {
					if !flanking(m) {
						p.Marks = append(p.Marks, m)
					}
				}
			}
			split = append(split, p)
		}
	}
	split = mergeText(split)
	// trim at line edges
	for i := range split {
		if split[i].Type != "text" {
			continue
		}
		edge := func(j int) bool {
			return j < 0 || j >= len(split) || split[j].Type == "hardBreak"
		}
		t := split[i].Text
		if edge(i - 1) {
			t = strings.TrimLeft(t, " \t")
		}
		if edge(i + 1) {
			t = strings.TrimRight(t, " \t")
		}
		cp := *split[i]
		cp.Text = t
		split[i] = &cp
	}
	return mergeText(split)
}

func spaceRuns(s string) []string {
	var out []string
	start := 0
	rs := []rune(s)
	for i := 1; i <= len(rs); i++ {
		if i == len(rs) || unicode.IsSpace(rs[i]) != unicode.IsSpace(rs[i-1]) {
			out = append(out, string(rs[start:i]))
			start = i
		}
	}
	return out
}

func semanticKey(ns []*Node) string {
	var b strings.Builder
	for _, n := range canon(ns) {
		b.WriteString(key(n))
	}
	return b.String()
}

type corpusStats struct {
	docs, exact, valid             int
	blocks, same, lossless, llSame int
	features                       map[string]int
	failures                       []string
}

// checkCorpus runs the three corpus properties over docs.
func checkCorpus(t *testing.T, docs []namedDoc, schemaToo bool) corpusStats {
	t.Helper()
	ctx := context.Background()
	st := corpusStats{features: map[string]int{}}
	fail := func(format string, args ...any) {
		st.failures = append(st.failures, fmt.Sprintf(format, args...))
	}
	var backs [][]byte
	var backNames []string
	for _, d := range docs {
		st.docs++
		md := ToMarkdown(d.doc)
		r := resolverFor(d.doc)
		edited, _, err := Edit(ctx, d.doc, md, r)
		switch {
		case err != nil:
			fail("%s: Edit: %v", d.name, err)
		case mustJSON(edited) != mustJSON(d.doc):
			fail("%s: Edit(doc, ToMarkdown(doc)) differs from doc", d.name)
		case r.calls != 0:
			fail("%s: Edit made %d resolver calls", d.name, r.calls)
		default:
			st.exact++
		}
		back, _, err := FromMarkdown(ctx, md, resolverFor(d.doc))
		if err != nil {
			fail("%s: FromMarkdown: %v", d.name, err)
		} else if err := Validate(back); err != nil {
			fail("%s: Validate: %v", d.name, err)
		} else if schemaToo && knownDeviation(back) == "" {
			// A layout with one column comes back as one; the product
			// writes those though the schema wants two or three.
			backs = append(backs, []byte(mustJSON(back)))
			backNames = append(backNames, d.name)
		} else {
			st.valid++
		}
		for i, b := range d.doc.Content {
			st.blocks++
			feats := lossy(b)
			for _, f := range feats {
				st.features[f.phrase]++
			}
			got, _, err := FromMarkdown(ctx, ToMarkdown(Doc(b)),
				resolverFor(d.doc))
			same := err == nil && semanticKey(got.Content) ==
				semanticKey([]*Node{b})
			if same {
				st.same++
			}
			if len(feats) == 0 {
				st.lossless++
				if same {
					st.llSame++
				} else {
					fail("%s: block %d (%s) is lossless but does not round "+
						"trip:\n%s", d.name, i, b.Type, ToMarkdown(Doc(b)))
				}
			}
		}
	}
	for i, err := range schemaAll(t, backs) {
		if err != nil {
			fail("%s: schema: %.300v", backNames[i], err)
		} else {
			st.valid++
		}
	}
	return st
}

func (st corpusStats) report(t *testing.T, label string) {
	t.Helper()
	pct := func(a, b int) float64 {
		if b == 0 {
			return 100
		}
		return 100 * float64(a) / float64(b)
	}
	t.Logf("%s: %d docs; Edit exact %d (%.1f%%); valid %d (%.1f%%); "+
		"blocks %d, semantic round trip %d (%.1f%%); lossless blocks %d, "+
		"round trip %d (%.1f%%)", label, st.docs, st.exact,
		pct(st.exact, st.docs), st.valid, pct(st.valid, st.docs), st.blocks,
		st.same, pct(st.same, st.blocks), st.lossless, st.llSame,
		pct(st.llSame, st.lossless))
	var fs []string
	for f, n := range st.features {
		fs = append(fs, fmt.Sprintf("%5d  %s", n, f))
	}
	sort.Strings(fs)
	t.Logf("%s: blocks with lossy constructs:\n%s", label,
		strings.Join(fs, "\n"))
	for i, f := range st.failures {
		if i == 30 {
			t.Errorf("... and %d more failures", len(st.failures)-30)
			break
		}
		t.Error(f)
	}
}

func TestCorpus(t *testing.T) {
	docs := loadDocs(t, corpusDir)
	if len(docs) == 0 {
		t.Fatal("no committed corpus in " + corpusDir)
	}
	for _, d := range docs {
		if err := Validate(d.doc); err != nil {
			t.Errorf("%s: corpus doc invalid: %v", d.name, err)
		}
	}
	checkCorpus(t, docs, true).report(t, "corpus")
}

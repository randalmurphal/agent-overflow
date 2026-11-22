package adf

import (
	"strings"
	"testing"
)

// TestLossSetBySite proves each place that makes a content-losing
// warning states how the lost content is written, from what it lost, on
// a real Edit. Several cases word their message so that matching it for
// "expand" or a marker word would give a different answer.
func TestLossSetBySite(t *testing.T) {
	p := func(s string) string {
		return `{"type":"paragraph","content":[{"type":"text","text":"` + s + `"}]}`
	}
	expand := func(title, attrs string, kids ...string) string {
		return `{"type":"expand","attrs":{"title":"` + title + `"}` + attrs + `,"content":[` +
			strings.Join(kids, ",") + `]}`
	}
	macro := func(key string, kids ...string) string {
		return `{"type":"bodiedExtension","attrs":{"extensionKey":"` + key +
			`","extensionType":"com.atlassian.confluence.macro.core"},"content":[` +
			strings.Join(kids, ",") + `]}`
	}
	col := func(kids ...string) string {
		return `{"type":"layoutColumn","attrs":{"width":50},"content":[` + strings.Join(kids, ",") + `]}`
	}
	layout := `{"type":"layoutSection","content":[` + col(expand("T", "", p("a"))) + `,` + col(p("b")) + `]}`
	doc := func(blocks ...string) string {
		blocks = append(append([]string{p("Intro")}, blocks...), p("End"))
		return `{"type":"doc","version":1,"content":[` + strings.Join(blocks, ",") + `]}`
	}
	// dropLines removes every line of md that starts with one of prefixes,
	// with the blank line after it.
	dropLines := func(prefixes ...string) func(string) string {
		return func(md string) string {
			var out []string
			lines := strings.Split(md, "\n")
			for i := 0; i < len(lines); i++ {
				drop := false
				for _, pre := range prefixes {
					drop = drop || strings.HasPrefix(lines[i], pre)
				}
				if drop {
					if i+1 < len(lines) && lines[i+1] == "" {
						i++
					}
					continue
				}
				out = append(out, lines[i])
			}
			return strings.Join(out, "\n")
		}
	}
	appendMD := func(s string) func(string) string {
		return func(md string) string { return md + "\n" + s }
	}
	for _, tc := range []struct {
		site, name string
		orig       string // doc JSON, or "golden:NAME" for a golden edit case
		edit       func(md string) string
		msg        string // the warning's message holds this
		want       LossSet
	}{
		{"part", "a table rebuilt from md", "golden:edit-table-span-row-moved", nil,
			"the table had", lossMarkers},
		{"part", "a cell rebuilt from md", "golden:edit-table-cell-rich-structural", nil,
			`the table cell "**Build host**" had`, lossMarkers},
		{"unclosed", "a macro keyed expand whose close was deleted",
			doc(macro("expand", p("inside"))), dropLines("<!-- /macro"),
			"<!-- macro: expand dd81 --> is never closed", lossMarkers},
		{"lozenge", "a lozenge renamed with other formatting", "golden:edit-status-text-with-format",
			nil, "is plain text now", lossMarkers},
		{"flattened", "a layout holding an expand, markers removed", doc(layout),
			dropLines("<!--"), "had a column layout", lossMarkers | lossDetails},
		{"flattened", "an expand holding a macro, every tag removed", doc(expand("Notes", "",
			macro("details", p("inside")))), dropLines("<details>", "</details>", "<!--"),
			"had a macro", lossMarkers | lossDetails},
		{"flattened", "a full-width expand, tags removed (message says layout)",
			doc(expand("T", `,"marks":[{"type":"breakout","attrs":{"mode":"wide"}}]`, p("x"))),
			dropLines("<details>", "</details>"), "had full-width layout", lossDetails},
		{"unbalanced", "a stray panel close", doc(), appendMD("<!-- /panel -->\n"),
			"<!-- /panel --> closes nothing", lossMarkers},
		{"unbalanced", "a panel never closed", doc(), appendMD("<!-- panel: custom -->\n\nx\n"),
			"<!-- panel: custom --> is never closed", lossMarkers},
		{"flat", "a column outside a layout", doc(),
			appendMD("<!-- column 50% -->\n\nx\n\n<!-- /column -->\n"),
			"is outside a <!-- layout -->", lossMarkers},
		{"macroMiss", "a mistyped ID on a macro keyed expand", doc(macro("expand", p("inside"))),
			func(md string) string {
				return strings.NewReplacer("<!-- macro: expand dd81 -->", "<!-- macro: expand ffff -->",
					"<!-- /macro: expand dd81 -->", "<!-- /macro: expand ffff -->").Replace(md)
			}, "matches none of the expand macros", lossMarkers},
		{"kept", "a custom panel lost to a tie", "golden:edit-panels-rewritten-tie", nil,
			"the custom panel is still in the Markdown", lossMarkers},
	} {
		var orig *Node
		var md string
		if name, ok := strings.CutPrefix(tc.orig, "golden:"); ok {
			orig = parseGoldenJSON(t, readGolden(t, name, ".orig.json"))
			md = readGolden(t, name, ".md")
		} else {
			orig = parseGoldenJSON(t, tc.orig)
			md = tc.edit(ToMarkdown(orig))
		}
		_, ws, err := Edit(t.Context(), orig, md, newFakeResolver())
		if err != nil {
			t.Errorf("%s, %s: %v", tc.site, tc.name, err)
			continue
		}
		found := false
		for _, w := range ws {
			if !strings.Contains(w.Msg, tc.msg) {
				continue
			}
			found = true
			if w.Loses != tc.want {
				t.Errorf("%s, %s: %q loses %v, want %v", tc.site, tc.name, w, w.Loses.Kinds(),
					tc.want.Kinds())
			}
		}
		if !found {
			t.Errorf("%s, %s: no warning holds %q in %v", tc.site, tc.name, tc.msg, ws)
		}
	}
}

// TestRichLossByNode proves the edit invariant's warning for a rich node
// the edit drops states the loss from the node: details for an expand,
// even one whose title holds marker words, markers for a macro, and none
// for dress.
func TestRichLossByNode(t *testing.T) {
	text := func(s, marks string) string {
		return `{"type":"text","text":"` + s + `"` + marks + `}`
	}
	p := func(kids ...string) string {
		return `{"type":"paragraph","content":[` + strings.Join(kids, ",") + `]}`
	}
	doc := func(block string) *Node {
		return parseGoldenJSON(t, `{"type":"doc","version":1,"content":[`+block+`,`+
			p(text("End", ""))+`]}`)
	}
	plain := p(text("body", ""))
	for _, tc := range []struct {
		name, orig, out, msg string
		want                 LossSet
	}{
		{"expand", `{"type":"expand","attrs":{"title":"Status macro date"},"content":[` + plain + `]}`,
			plain, `the expand "Status macro date"`, lossDetails},
		{"macro", `{"type":"extension","attrs":{"extensionKey":"expand","extensionType":"x"}}`,
			plain, `the macro "expand"`, lossMarkers},
		{"text color", p(text("body", `,"marks":[{"type":"textColor","attrs":{"color":"#ff0000"}}]`)),
			plain, "text color", 0},
	} {
		orig := doc(tc.orig)
		ws := checkKeptMD(orig, doc(tc.out), ToMarkdown(orig), nil)
		found := false
		for _, w := range ws {
			if strings.Contains(w.Msg, tc.msg) {
				found = true
				if w.Loses != tc.want {
					t.Errorf("%s: %q loses %v, want %v", tc.name, w, w.Loses.Kinds(), tc.want.Kinds())
				}
			}
		}
		if !found {
			t.Errorf("%s: no warning holds %q in %v", tc.name, tc.msg, ws)
		}
	}
}

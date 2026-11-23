package adf

import (
	"context"
	"slices"
	"strings"
	"testing"
)

// TestCheckKeptNamesUnexplainedLoss proves the invariant flags a rich
// node the Markdown still shows but the result lacks, here a macro whose
// parameters changed under an unchanged marker, naming it and its line,
// and stays quiet once the marker is removed.
func TestCheckKeptNamesUnexplainedLoss(t *testing.T) {
	toc := func(level string) *Node {
		return &Node{Type: "extension", Attrs: map[string]any{"extensionKey": "toc",
			"extensionType": "x", "parameters": map[string]any{"maxLevel": level}}}
	}
	orig := Doc(para("Intro"), toc("2"))
	md := ToMarkdown(orig)
	ws := checkKeptMD(orig, Doc(para("Intro"), toc("3")), md, nil)
	if len(ws) != 1 || !ws[0].Flattens() || ws[0].Line != 3 ||
		ws[0].Msg != `the macro "toc" is still in the Markdown, but the edit would lose it` {
		t.Errorf("changed parameters: %v", ws)
	}
	if ws := checkKeptMD(orig, Doc(para("Intro")), "Intro\n", nil); len(ws) != 0 {
		t.Errorf("removed marker: %v", ws)
	}
	if ws := checkKeptMD(orig, Doc(para("Intro")), md, map[*Node]bool{orig.Content[1]: true}); len(ws) != 0 {
		t.Errorf("already warned: %v", ws)
	}
}

// TestCheckKeptCellSettingsFollowText proves cell settings belong to the
// cell's text: two unchanged cells that trade colors are a loss (a
// warning, as colors are dress), while an
// edited cell that keeps its color, even when the old text still shows
// inside the new, is not.
func TestCheckKeptCellSettingsFollowText(t *testing.T) {
	cell := func(text, color string) *Node {
		return &Node{Type: "tableCell", Attrs: map[string]any{"background": color},
			Content: []*Node{para(text)}}
	}
	table := func(a, b *Node) *Node {
		return Doc(&Node{Type: "table", Content: []*Node{
			{Type: "tableRow", Content: []*Node{
				{Type: "tableHeader", Content: []*Node{para("Item")}},
				{Type: "tableHeader", Content: []*Node{para("State")}}}},
			{Type: "tableRow", Content: []*Node{a, b}}}})
	}
	orig := table(cell("Build", "#e3fcef"), cell("pending", "#ffebe6"))
	md := ToMarkdown(orig)
	swapped := table(cell("Build", "#ffebe6"), cell("pending", "#e3fcef"))
	if ws := checkKeptMD(orig, swapped, md, nil); len(ws) != 2 || ws[0].Flattens() {
		t.Errorf("traded colors, dress, warn without refusing: %v", ws)
	}
	edited := strings.Replace(md, "pending", "pending review", 1)
	kept := table(cell("Build", "#e3fcef"), cell("pending review", "#ffebe6"))
	if ws := checkKeptMD(orig, kept, edited, nil); len(ws) != 0 {
		t.Errorf("edited cell keeping its color: %v", ws)
	}
	lostColor := table(cell("Build", "#e3fcef"), cell("pending", "#e3fcef"))
	if ws := checkKeptMD(orig, lostColor, md, nil); len(ws) != 1 {
		t.Errorf("unchanged cell taking another's color: %v", ws)
	}
}

// TestInventoryCoversLossyData proves every node in the corpus with a
// data feature Markdown drops (see payloadFeatures) has an entry in the
// invariant's inventory, so its loss can't pass unseen.
func TestInventoryCoversLossyData(t *testing.T) {
	docs := loadDocs(t, corpusDir)
	seen := map[string]int{}
	for _, d := range docs {
		have := map[*Node]bool{}
		for _, r := range inventory(d.doc) {
			have[r.n] = true
		}
		walk(d.doc, func(n, parent *Node) {
			node, marks := payloadFeatures(n, parent)
			if len(node) == 0 && len(marks) == 0 {
				return
			}
			for _, p := range node {
				seen[p]++
			}
			for _, ps := range marks {
				for _, p := range ps {
					seen[p]++
				}
			}
			if !have[n] {
				t.Errorf("%s: %s with %v %v has no inventory entry", d.name, n.Type, node, marks)
			}
		})
	}
	t.Logf("lossy data features inventoried: %v", seen)
}

// TestWordMarksStayOffRewrites proves a color or inline comment stays on
// its words through a word edit and comes off a rewrite, on both the
// patch and the rebuild: an inline comment taken off is refused.
func TestWordMarksStayOffRewrites(t *testing.T) {
	red := Mark{Type: "textColor", Attrs: map[string]any{"color": "#ff5630"}}
	note := Mark{Type: "annotation", Attrs: map[string]any{
		"id": "c0ffee00-1111-2222-3333-444455556666", "annotationType": "inlineComment"}}
	cases := []struct {
		mark     Mark
		text, md string
		kept     string // the text left with the mark, "" for none
		refused  bool
		rebuilds bool // the edit also adds a paragraph, so the block rebuilds
	}{
		{red, "Total amount due", "Total amount due now", "Total amount due", false, false},
		{red, "Total amount due", "Total amont due", "Total amont due", false, false},
		{red, "Owner Bob on call", "Owner Alice on call", "Owner|on call", false, false},
		{red, "blocked", "blocking", "blocking", false, false},
		{red, "Total amount due", "Tot is now rewritten", "", false, false},
		{red, "Total amount due", "Tot is now rewritten", "", false, true},
		{red, "now", "later", "", false, false},
		{note, "Total amount due", "Total amont due", "Total amont due", false, false},
		{note, "Total amount due", "Total amount is due", "", false, false},
		{note, "Total amount due", "Total amount is due", "", false, true},
		{note, "Total amount due", "Something else", "", false, false},
	}
	for _, c := range cases {
		doc := Doc(&Node{Type: "paragraph", Content: []*Node{
			{Type: "text", Text: "Intro "}, {Type: "text", Text: c.text, Marks: []Mark{c.mark}}}},
			&Node{Type: "paragraph", Content: []*Node{{Type: "text", Text: "End"}}})
		md := "Intro " + c.md + "\n\nEnd\n"
		if c.rebuilds {
			md = "Intro " + c.md + "\n\nNew\n\nEnd\n"
		}
		out, ws, err := Edit(context.Background(), doc, md, resolverFor(doc))
		if err != nil {
			t.Fatalf("%q to %q: %v", c.text, c.md, err)
		}
		refused := slices.ContainsFunc(ws, func(w Warning) bool { return w.Flattens() })
		dropped := slices.ContainsFunc(ws, func(w Warning) bool {
			return strings.HasPrefix(w.Msg, "dropped the inline comments")
		})
		if c.mark.Type == "annotation" && c.kept == "" && !dropped {
			t.Errorf("%q to %q (rebuild %v): no warning names the dropped comment: %v",
				c.text, c.md, c.rebuilds, ws)
		}
		var kept []string
		walk(out, func(n, _ *Node) {
			if n.Type == "text" && slices.ContainsFunc(n.Marks, func(m Mark) bool {
				return m.Type == c.mark.Type
			}) {
				kept = append(kept, n.Text)
			}
		})
		if got := strings.Join(kept, "|"); got != c.kept || refused != c.refused {
			t.Errorf("%s %q to %q (rebuild %v): marked %q, refused %v; want %q, %v",
				c.mark.Type, c.text, c.md, c.rebuilds, got, refused, c.kept, c.refused)
		}
	}
}

// TestCheckKeptStateStaysWithItsText proves hidden state belongs to the
// text it was on: two decisions that trade states, and a color moved
// from text that still shows onto another's first letter, are losses: the
// state refused, the color, dress, warned.
func TestCheckKeptStateStaysWithItsText(t *testing.T) {
	decisions := func(a, b string) *Node {
		item := func(text, state string) *Node {
			return &Node{Type: "decisionItem", Attrs: map[string]any{"state": state},
				Content: []*Node{{Type: "text", Text: text}}}
		}
		return Doc(&Node{Type: "decisionList", Content: []*Node{
			item("Use Go", a), item("Ship weekly", b)}})
	}
	orig := decisions("DECIDED", "UNDECIDED")
	if ws := checkKeptMD(orig, decisions("UNDECIDED", "DECIDED"), ToMarkdown(orig), nil); len(ws) == 0 ||
		!ws[0].Flattens() {
		t.Errorf("decisions that traded states: %v", ws)
	}
	red := []Mark{{Type: "textColor", Attrs: map[string]any{"color": "#bf2600"}}}
	colored := Doc(&Node{Type: "paragraph", Content: []*Node{
		{Type: "text", Text: "BUG NOTED", Marks: red}, {Type: "text", Text: " in the parser"}}},
		para("Both agree"))
	moved := Doc(para("Both agree"), &Node{Type: "paragraph", Content: []*Node{
		{Type: "text", Text: "B", Marks: red}, {Type: "text", Text: "UG NOTED in the parser"}}})
	md := "Both agree\n\nBUG NOTED in the parser\n"
	if ws := checkKeptMD(colored, moved, md, nil); len(ws) != 1 || ws[0].Flattens() {
		t.Errorf("a color moved onto a fragment: %v", ws)
	}
}

// namesContent reports whether a Flattens warning names content: a
// container or macro that can't be kept, or a lost node or feature that
// isn't dress (see cosmeticPhrases). Every refusal must, so dress alone
// never stops an edit.
func namesContent(w Warning) bool {
	m := w.Msg
	for _, s := range []string{"its container is not kept", "its macro is not kept",
		"matches none of the", "is plain text now"} {
		if strings.Contains(m, s) {
			return true
		}
	}
	if _, rest, ok := strings.Cut(m, " had "); ok {
		feats, _, _ := strings.Cut(rest, ", which the edit")
		parts := strings.Split(strings.ReplaceAll(feats, " and ", ", "), ", ")
		return !cosmetic(parts...)
	}
	for _, dress := range []string{"the cell formatting", "the table's settings",
		"the image dimensions", "dropped "} {
		if strings.HasPrefix(m, dress) {
			return false
		}
	}
	for p := range cosmeticPhrases.get() {
		if strings.HasPrefix(m, "the "+p+" of ") {
			return false
		}
	}
	return true
}

// TestDressLossWarnsContentLossRefuses proves the line between dress and
// content: a result that drops dress Markdown can't show (a color, an
// alignment, a width, a comment) warns, naming it, and the edit goes
// ahead; one that drops a content node the Markdown still shows refuses.
func TestDressLossWarnsContentLossRefuses(t *testing.T) {
	text := func(s, marks string) string {
		return `{"type":"text","text":"` + s + `","marks":[` + marks + `]}`
	}
	p := func(inline string, marks string) string {
		return `{"type":"paragraph","content":[` + inline + `],"marks":[` + marks + `]}`
	}
	plain := p(text("Alert raised", ""), "")
	img := func(single, media, marks string) string {
		if single == "" {
			single = `"layout":"center"`
		}
		return `{"type":"mediaSingle","attrs":{` + single + `},"content":[{"type":"media",` +
			`"attrs":{"type":"file","id":"abc-1","collection":"c"` + media + `},"marks":[` +
			marks + `]}]}`
	}
	table := func(attrs, cell string) string {
		return `{"type":"table","attrs":{` + attrs + `},"content":[{"type":"tableRow","content":[` +
			`{"type":"tableHeader","content":[` + p(text("Item", ""), "") + `]},` +
			`{"type":"tableHeader","content":[` + p(text("State", ""), "") + `]}]},` +
			`{"type":"tableRow","content":[{"type":"tableCell","attrs":{` + cell + `},"content":[` +
			p(text("Build", ""), "") + `]},{"type":"tableCell","content":[` +
			p(text("done", ""), "") + `]}]}]}`
	}
	code := func(attrs, marks string) string {
		return `{"type":"codeBlock","attrs":{"language":"go"` + attrs + `},"marks":[` + marks +
			`],"content":[{"type":"text","text":"x := 1"}]}`
	}
	inline := func(node string) string {
		return p(text("See ", "")+","+node+","+text(" now", ""), "")
	}
	cases := []struct {
		name, orig, out string
		content         bool
	}{
		{"text color", p(text("Alert raised", `{"type":"textColor","attrs":{"color":"#bf2600"}}`), ""),
			plain, false},
		{"highlighting", p(text("Alert raised",
			`{"type":"backgroundColor","attrs":{"color":"#fffae6"}}`), ""), plain, false},
		{"inline comments", p(text("Alert raised", `{"type":"annotation","attrs":`+
			`{"id":"c0ffee00-1111-2222-3333-444455556666","annotationType":"inlineComment"}}`), ""),
			plain, false},
		{"alignment", p(text("Alert raised", ""), `{"type":"alignment","attrs":{"align":"center"}}`),
			plain, false},
		{"indentation", p(text("Alert raised", ""), `{"type":"indentation","attrs":{"level":1}}`),
			plain, false},
		{"link details", p(text("Alert raised", `{"type":"link","attrs":`+
			`{"href":"https://example.com","id":"x1"}}`), ""), p(text("Alert raised",
			`{"type":"link","attrs":{"href":"https://example.com"}}`), ""), false},
		{"code block settings", code(`,"wrap":true`, ""), code("", ""), false},
		{"full-width layout", code("", `{"type":"breakout","attrs":{"mode":"wide"}}`),
			code("", ""), false},
		{"table width or layout", table(`"layout":"wide"`, ""), table("", ""), false},
		{"table column widths", table("", `"colwidth":[120]`), table("", ""), false},
		{"cell colors", table("", `"background":"#e3fcef"`), table("", ""), false},
		{"image size or position", img(`"layout":"wrap-left","width":50`, "", ""), img("", "", ""),
			false},
		{"image dimensions", img("", `,"width":640,"height":480`, ""), img("", "", ""), false},
		{"image borders", img("", "", `{"type":"border","attrs":{"size":2,"color":"#091e4224"}}`),
			img("", "", ""), false},
		{"merged cells", table("", `"colspan":1,"rowspan":2`), table("", ""), true},
		{"status lozenge", inline(`{"type":"status","attrs":{"text":"DONE","color":"green"}}`),
			p(text("See [DONE] now", ""), ""), true},
		{"mention", inline(`{"type":"mention","attrs":{"id":"acc-1","text":"@User 5"}}`),
			p(text("See @User 5 now", ""), ""), true},
		{"date", inline(`{"type":"date","attrs":{"timestamp":"1790812800000"}}`),
			p(text("See 2026-10-01 now", ""), ""), true},
		{"emoji", inline(`{"type":"emoji","attrs":{"shortName":":smile:","text":"😄"}}`),
			p(text("See 😄 now", ""), ""), true},
		{"macro", `{"type":"extension","attrs":{"extensionKey":"toc","extensionType":"x",` +
			`"parameters":{"maxLevel":"2"}}}`, plain, true},
		{"panel", `{"type":"panel","attrs":{"panelType":"warning"},"content":[` + plain + `]}`,
			plain, true},
		{"expand", `{"type":"expand","attrs":{"title":"More"},"content":[` + plain + `]}`,
			plain, true},
		{"image", img("", "", ""), plain, true},
		{"link card", inline(`{"type":"inlineCard","attrs":{"url":"https://example.com/x"}}`),
			p(text("See https://example.com/x now", ""), ""), true},
		{"decision", `{"type":"decisionList","attrs":{"localId":"d"},"content":[{"type":` +
			`"decisionItem","attrs":{"localId":"i","state":"DECIDED"},"content":[` +
			text("Ship it", "") + `]}]}`, p(text("Decision: Ship it", ""), ""), true},
	}
	doc := func(block string) *Node {
		return parseGoldenJSON(t, `{"type":"doc","version":1,"content":[`+block+`,`+
			p(text("End", ""), "")+`]}`)
	}
	for _, c := range cases {
		orig, out := doc(c.orig), doc(c.out)
		if err := Validate(orig); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		ws := checkKeptMD(orig, out, ToMarkdown(orig), nil)
		if len(ws) == 0 {
			t.Errorf("%s: lost unwarned", c.name)
			continue
		}
		for _, w := range ws {
			if w.Flattens() != c.content || w.Flattens() != namesContent(w) {
				t.Errorf("%s: %v refuses %v; want %v", c.name, w, w.Flattens(), c.content)
			}
		}
	}
}

// inventory lists the rich nodes in doc, in document order.
func inventory(doc *Node) []rich { return inventorySkip(doc, nil) }

package adf

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestPlainText(t *testing.T) {
	md := "# Title\n\nHello **bold** @[Jane Doe]\\\nnext\n\n" +
		"- one\n- two\n\n| a | b |\n| --- | --- |\n| 1 | 2 |\n\n" +
		"<details><summary>More</summary>\n\n```\ncode\n```\n\n</details>\n"
	doc, _, err := FromMarkdown(context.Background(), md, newFakeResolver())
	if err != nil {
		t.Fatal(err)
	}
	want := "Title\nHello bold @Jane Doe\nnext\none\ntwo\na\tb\n1\t2\nMore\ncode"
	if got := PlainText(doc); got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
	if PlainText(nil) != "" {
		t.Error("PlainText(nil) should be empty")
	}
}

func TestEmptyDocMarshalsContent(t *testing.T) {
	doc, _, err := FromMarkdown(context.Background(), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := mustJSON(doc); got != `{"type":"doc","version":1,"content":[]}` {
		t.Errorf("got %s", got)
	}
	if got := mustJSON(&Node{Type: "listItem"}); got !=
		`{"type":"listItem","content":[]}` {
		t.Errorf("got %s", got)
	}
	if got := mustJSON(&Node{Type: "paragraph"}); got != `{"type":"paragraph"}` {
		t.Errorf("got %s", got)
	}
}

func TestInvalidMarkdownDoesNotUploadEarlierImages(t *testing.T) {
	for _, md := range []string{
		"![image](local.png)\n\n<div>unsupported</div>",
		"![image](local.png)\n\n@[accountid:]",
		"![image](local.png)\n\n- item\n\n  | a |\n  | --- |\n  | b |",
	} {
		r := newFakeResolver()
		_, _, err := FromMarkdown(t.Context(), md, r)
		if err == nil || len(r.uploads) != 0 {
			t.Fatalf("uploaded before validating the document: %v %v", r.uploads, err)
		}
	}
}

func TestUnresolvableMentionDoesNotUploadEarlierImages(t *testing.T) {
	r := newFakeResolver()
	_, _, err := FromMarkdown(t.Context(), "![image](local.png)\n\n@[Unknown Person]", r)
	if err == nil || len(r.uploads) != 0 {
		t.Fatalf("uploaded before resolving mentions: %v %v", r.uploads, err)
	}
}

func TestEditNilOrig(t *testing.T) {
	doc, _, err := Edit(context.Background(), nil, "hi", nil)
	if err != nil || len(doc.Content) != 1 {
		t.Fatalf("Edit(nil): %v %v", doc, err)
	}
}

func TestCRLF(t *testing.T) {
	doc, _, err := FromMarkdown(context.Background(),
		"a\r\nb\r\n\r\n- c\r\n", nil)
	if err != nil {
		t.Fatal(err)
	}
	if js := mustJSON(doc); strings.Contains(js, `\r`) {
		t.Errorf("carriage return leaked into ADF: %s", js)
	}
}

// JSON like "content":[null] decodes to nil nodes. Validate reports
// them; the read and edit paths skip them.
func TestNilNodes(t *testing.T) {
	var doc Node
	js := `{"type":"doc","version":1,"content":[null,{"type":"paragraph",` +
		`"content":[null,{"type":"text","text":"hi"}]},{"type":"table",` +
		`"content":[null,{"type":"tableRow","content":[null]}]}]}`
	if err := json.Unmarshal([]byte(js), &doc); err != nil {
		t.Fatal(err)
	}
	if got := ToMarkdown(&doc); got != "hi\n" {
		t.Errorf("ToMarkdown: got %q", got)
	}
	if got := PlainText(&doc); got != "hi" {
		t.Errorf("PlainText: got %q", got)
	}
	if err := Validate(&doc); err == nil ||
		!strings.Contains(err.Error(), "content[0]: nil node") {
		t.Errorf("Validate: %v", err)
	}
	edited, _, err := Edit(context.Background(), &doc, "hi\n", nil)
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if strings.Contains(mustJSON(edited), "null") {
		t.Errorf("Edit kept nil nodes: %s", mustJSON(edited))
	}
}

func TestValidatePath(t *testing.T) {
	doc := Doc(&Node{Type: "bulletList", Content: []*Node{
		{Type: "listItem", Content: []*Node{{Type: "paragraph"}}},
		{Type: "listItem", Content: []*Node{{Type: "rule"}}},
	}})
	err := Validate(doc)
	want := "adf: content[0].content[1]: listItem cannot contain rule"
	if err == nil || err.Error() != want {
		t.Errorf("got %v, want %s", err, want)
	}
	if err := Validate(&Node{Type: "paragraph"}); err == nil ||
		err.Error() != `adf: doc: root is "paragraph", want doc` {
		t.Errorf("root: %v", err)
	}
}

func TestCanceledConversionsDoNotResolve(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	orig := Doc(&Node{Type: "paragraph", Content: []*Node{{Type: "text", Text: "original"}}})
	r := newFakeResolver()
	for _, convert := range []func() error{
		func() error { _, _, err := FromMarkdown(ctx, "![image](local.png)", r); return err },
		func() error { _, _, err := Edit(ctx, orig, "![image](local.png)", r); return err },
	} {
		if err := convert(); !errors.Is(err, context.Canceled) {
			t.Fatalf("lost cancellation: %v", err)
		}
	}
	if len(r.uploads) != 0 {
		t.Fatalf("uploads: %v", r.uploads)
	}
}

func TestEqual(t *testing.T) {
	var parsed Node
	if err := json.Unmarshal([]byte(`{"type":"doc","version":1,"content":`+
		`[{"type":"heading","attrs":{"level":2},"content":[{"type":"text",`+
		`"text":"Hi"}]}]}`), &parsed); err != nil {
		t.Fatal(err)
	}
	built, _, err := FromMarkdown(context.Background(), "## Hi\n", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !Equal(&parsed, built) {
		t.Errorf("parsed and built docs differ: %s vs %s", mustJSON(&parsed),
			mustJSON(built))
	}
	if !Equal(nil, Doc()) || Equal(nil, built) {
		t.Error("nil should equal only an empty doc")
	}
	other, _, _ := FromMarkdown(context.Background(), "## Ho\n", nil)
	if Equal(built, other) {
		t.Error("different text compared equal")
	}
}

// TestTagShapedProseIsText proves generics and other tag-shaped prose are
// literal text both ways, while block-level HTML stays an error.
func TestTagShapedProseIsText(t *testing.T) {
	ctx := context.Background()
	for _, text := range []string{"List<String>", "Map<K, V>", "a <b>bold</b> claim",
		"Optional<List<T>> and </T>"} {
		md := text + "\n"
		doc, _, err := FromMarkdown(ctx, md, nil)
		if err != nil {
			t.Fatalf("%q: %v", text, err)
		}
		if got := PlainText(doc); got != text {
			t.Errorf("%q parsed as %q", text, got)
		}
		orig := Doc(&Node{Type: "paragraph", Content: []*Node{textNode(text)}})
		back, _, err := FromMarkdown(ctx, ToMarkdown(orig), nil)
		if err != nil || !Equal(back, orig) {
			t.Errorf("%q round trip via %q: %v %s", text, ToMarkdown(orig), err, mustJSON(back))
		}
	}
	// A literal tag across a soft line break keeps a space, not a newline.
	for _, md := range []string{"Use <T extends\nComparable> here\n", "Use <T extends\r\nComparable> here\r\n"} {
		doc, _, err := FromMarkdown(ctx, md, nil)
		if err != nil || PlainText(doc) != "Use <T extends Comparable> here" {
			t.Errorf("%q: %q %v", md, PlainText(doc), err)
		}
	}
	if _, _, err := FromMarkdown(ctx, "<div>\nblock\n</div>\n", nil); err == nil ||
		!strings.Contains(err.Error(), "HTML blocks may only be") {
		t.Errorf("block HTML: %v", err)
	}
}

// TestBreakOnlyParagraphsNeedNoGuard proves a paragraph holding only hard
// breaks renders as bare <br> tags, in any container, and reads back.
func TestBreakOnlyParagraphsNeedNoGuard(t *testing.T) {
	br := func(n int) *Node {
		p := &Node{Type: "paragraph"}
		for range n {
			p.Content = append(p.Content, &Node{Type: "hardBreak"})
		}
		return p
	}
	text := &Node{Type: "paragraph", Content: []*Node{textNode("x")}}
	docs := []*Node{
		Doc(text, br(1), text),
		Doc(br(2)),
		Doc(&Node{Type: "bulletList", Content: []*Node{{Type: "listItem", Content: []*Node{br(1)}}}}),
		Doc(&Node{Type: "blockquote", Content: []*Node{br(1)}}),
	}
	for _, doc := range docs {
		md := ToMarkdown(doc)
		if strings.Contains(md, "&#32;") {
			t.Errorf("guarded break-only paragraph: %q", md)
		}
		back, _, err := FromMarkdown(context.Background(), md, nil)
		if err != nil || !Equal(back, doc) {
			t.Errorf("%q: %v\n got %s\nwant %s", md, err, mustJSON(back), mustJSON(doc))
		}
	}
	if md := ToMarkdown(docs[0]); md != "x\n\n<br>\n\nx\n" {
		t.Errorf("got %q", md)
	}
}

// TestAnnotationOnInlineNodes proves an inline comment on a status or an
// emoji validates, so editing a block beside it still works.
func TestAnnotationOnInlineNodes(t *testing.T) {
	ann := []Mark{{Type: "annotation", Attrs: map[string]any{"id": "a1",
		"annotationType": "inlineComment"}}}
	para := &Node{Type: "paragraph", Content: []*Node{
		{Type: "status", Attrs: map[string]any{"text": "DONE", "color": "green"}, Marks: ann},
		{Type: "emoji", Attrs: map[string]any{"shortName": ":smile:"}, Marks: ann},
		{Type: "mention", Attrs: map[string]any{"id": "u1"}, Marks: ann},
	}}
	orig := Doc(para, &Node{Type: "paragraph", Content: []*Node{textNode("old")}})
	if err := Validate(orig); err != nil {
		t.Fatal(err)
	}
	md := ToMarkdown(orig)
	edited, _, err := Edit(context.Background(), orig, strings.Replace(md, "old", "new", 1), newFakeResolver())
	if err != nil || Validate(edited) != nil || !Equal(&Node{Type: "doc", Version: 1,
		Content: edited.Content[:1]}, Doc(para)) {
		t.Fatalf("edit beside annotated inline nodes: %v %s", err, mustJSON(edited))
	}
	bad := Doc(&Node{Type: "paragraph", Content: []*Node{{Type: "status",
		Attrs: map[string]any{"text": "X", "color": "red"}, Marks: []Mark{{Type: "strong"}}}}})
	if Validate(bad) == nil {
		t.Error("strong on a status validated")
	}
}

// TestBrLinesInAnyLayoutAreBreaks proves an HTML block that starts with
// <br> reads as a paragraph of breaks and text: CRLF endings, <br> lines
// with no blank line between them, and text after a <br>, without the
// space the view hides.
func TestBrLinesInAnyLayoutAreBreaks(t *testing.T) {
	for md, want := range map[string]string{
		"<br>\r\n":                "hardBreak",
		"<br>\n<br>\n":            "hardBreak hardBreak",
		"<br>\r\n<br/>\r\n":       "hardBreak hardBreak",
		"<br>\nnext line\n":       "hardBreak text:next line",
		"<br> then text\n":        "hardBreak text:then text",
		"a\n\n<br>\n<br>\nb\n\nc": "text:a | hardBreak hardBreak text:b | text:c",
	} {
		doc, _, err := FromMarkdown(context.Background(), md, nil)
		if err != nil {
			t.Errorf("%q: %v", md, err)
			continue
		}
		var blocks []string
		for _, b := range doc.Content {
			var parts []string
			for _, n := range b.Content {
				if n.Type == "text" {
					parts = append(parts, "text:"+n.Text)
				} else {
					parts = append(parts, n.Type)
				}
			}
			blocks = append(blocks, strings.Join(parts, " "))
		}
		if got := strings.Join(blocks, " | "); got != want {
			t.Errorf("%q: got %q, want %q", md, got, want)
		}
	}
}

// TestExternalImagesNeverUpload proves an external image whose URL reads
// as a local path round-trips as external media, so editing its block
// can't upload the file it names; any URL with a scheme is external too.
func TestExternalImagesNeverUpload(t *testing.T) {
	for _, url := range []string{"/home/me/.config/app/credentials.json", "pic.png",
		"ftp://host/a.png", "data:image/png;base64,AAAA", "https://e.com/a.png"} {
		doc := Doc(&Node{Type: "mediaSingle", Attrs: map[string]any{"layout": "center"},
			Content: []*Node{{Type: "media", Attrs: map[string]any{"type": "external", "url": url}}}})
		md := ToMarkdown(doc)
		r := newFakeResolver()
		back, _, err := FromMarkdown(context.Background(), md, r)
		if err != nil || len(r.uploads) != 0 {
			t.Errorf("%s: md %q err %v uploads %v", url, md, err, r.uploads)
			continue
		}
		if got, want := mustJSON(back.Content[0].Content[0].Attrs), mustJSON(doc.Content[0].Content[0].Attrs); got != want {
			t.Errorf("%s: md %q read back as %s", url, md, got)
		}
	}
	r := newFakeResolver()
	if _, _, err := FromMarkdown(context.Background(), "![](external:)\n", r); err == nil {
		t.Error("external: with no URL parsed")
	}
}

// TestEditMarksFlattenedContainers proves Edit flags only a container
// whose markers were removed or broken: an edit inside one, or one
// removed whole, flattens nothing.
func TestEditMarksFlattenedContainers(t *testing.T) {
	para := func(s string) *Node {
		return &Node{Type: "paragraph", Content: []*Node{{Type: "text", Text: s}}}
	}
	layout := &Node{Type: "layoutSection", Content: []*Node{
		{Type: "layoutColumn", Attrs: map[string]any{"width": 50.0}, Content: []*Node{para("left")}},
		{Type: "layoutColumn", Attrs: map[string]any{"width": 50.0}, Content: []*Node{para("right")}}}}
	bodied := &Node{Type: "bodiedExtension", Attrs: map[string]any{"extensionKey": "details",
		"extensionType": "com.atlassian.confluence.macro.core"}, Content: []*Node{para("inside")}}
	custom := &Node{Type: "panel", Attrs: map[string]any{"panelType": "custom", "panelColor": "#ff0000"},
		Content: []*Node{para("note")}}
	colored := &Node{Type: "paragraph", Content: []*Node{{Type: "text", Text: "red",
		Marks: []Mark{{Type: "textColor", Attrs: map[string]any{"color": "#ff0000"}}}}}}
	drop := func(marks ...string) func(string) string {
		return func(md string) string {
			for _, m := range marks {
				md = strings.Replace(md, m+"\n\n", "", 1)
			}
			return md
		}
	}
	for name, tc := range map[string]struct {
		block *Node
		edit  func(md string) string
		want  string // a Flattens warning containing this, or "" for none
	}{
		"layout edited inside": {layout, func(md string) string { return strings.Replace(md, "left", "LEFT", 1) }, ""},
		"macro edited inside":  {bodied, func(md string) string { return strings.Replace(md, "inside", "INSIDE", 1) }, ""},
		"panel edited inside":  {custom, func(md string) string { return strings.Replace(md, "note", "NOTE", 1) }, ""},
		"layout removed":       {layout, func(string) string { return "intro\n" }, ""},
		"color changed":        {colored, func(md string) string { return strings.Replace(md, "red", "RED", 1) }, ""},
		"layout markers removed": {layout, drop("<!-- layout -->", "<!-- column 50% -->", "<!-- /column -->",
			"<!-- column 50% -->", "<!-- /column -->", "<!-- /layout -->"), "a column layout"},
		"layout never closed": {layout, func(md string) string { return strings.Replace(md, "<!-- /layout -->", "", 1) },
			"line 3: <!-- layout --> is never closed with <!-- /layout -->"},
		"column closes nothing": {layout, func(md string) string {
			return strings.Replace(md, "<!-- column 50% -->\n\nright", "right", 1)
		}, "<!-- /column --> closes nothing"},
		"macro never closed": {bodied, func(md string) string { return strings.Replace(md, "<!-- /macro: details 79ac -->", "", 1) },
			"line 3: <!-- macro: details 79ac --> is never closed with <!-- /macro: details 79ac -->"},
		"panel markers removed": {custom, drop("<!-- panel: custom -->", "<!-- /panel -->"), "a custom panel"},
	} {
		doc := Doc(para("intro"), tc.block)
		md := ToMarkdown(doc)
		edited := tc.edit(md)
		if edited == md {
			t.Fatalf("%s: edit changed nothing in %q", name, md)
		}
		_, ws, err := Edit(context.Background(), doc, edited, newFakeResolver())
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var flat []string
		for _, w := range ws {
			if w.Flattens {
				flat = append(flat, w.String())
			}
		}
		got := strings.Join(flat, "; ")
		if tc.want == "" && got != "" || tc.want != "" && !strings.Contains(got, tc.want) {
			t.Errorf("%s: flattening warnings %q, want %q (all: %v)\n%s", name, got, tc.want, ws, edited)
		}
	}
}

// TestEditInsideContainerChangesOnlyThatBlock proves a one-word edit in a
// layout column keeps the layout, both columns and every other block as
// they were (attributes and localIds included) and changes only the
// edited paragraph.
func TestEditInsideContainerChangesOnlyThatBlock(t *testing.T) {
	para := func(s, id string) *Node {
		return &Node{Type: "paragraph", Attrs: map[string]any{"localId": id},
			Content: []*Node{{Type: "text", Text: s}}}
	}
	doc := Doc(para("intro", "p0"), &Node{Type: "layoutSection",
		Attrs: map[string]any{"localId": "L"}, Marks: []Mark{{Type: "breakout",
			Attrs: map[string]any{"mode": "wide"}}},
		Content: []*Node{
			{Type: "layoutColumn", Attrs: map[string]any{"width": 33.33, "localId": "c1"},
				Content: []*Node{para("one", "p1"), para("two words", "p2")}},
			{Type: "layoutColumn", Attrs: map[string]any{"width": 66.67, "localId": "c2"},
				Content: []*Node{{Type: "extension", Attrs: map[string]any{"extensionKey": "toc",
					"extensionType": "com.atlassian.confluence.macro.core"}}, para("three", "p3")}}}})
	md := ToMarkdown(doc)
	edited := strings.Replace(md, "two words", "two changed", 1)
	got, ws, err := Edit(context.Background(), doc, edited, newFakeResolver())
	if err != nil || len(ws) != 0 {
		t.Fatalf("%v %v", err, ws)
	}
	want := Doc(doc.Content[0], doc.Content[1])
	cols := got.Content[1].Content
	if p := cols[0].Content[1]; p.Content[0].Text != "two changed" {
		t.Fatalf("edit lost: %s", mustJSON(got))
	}
	// Everything but that paragraph is the original node.
	if got.Content[0] != want.Content[0] || cols[0].Content[0] != doc.Content[1].Content[0].Content[0] ||
		cols[1].Content[0] != doc.Content[1].Content[1].Content[0] ||
		cols[1].Content[1] != doc.Content[1].Content[1].Content[1] {
		t.Errorf("untouched blocks were rebuilt: %s", mustJSON(got))
	}
	orig := mustJSON(doc)
	if back := strings.Replace(mustJSON(got), `"text":"two changed"`, `"text":"two words"`, 1); !strings.Contains(back, `"localId":"L"`) ||
		!strings.Contains(back, `"mode":"wide"`) || !strings.Contains(back, `"width":33.33`) {
		t.Errorf("layout attributes lost:\n%s\n%s", back, orig)
	}
}

// TestCellItemsTakeOneSpace proves the spaces after a list marker at the
// start of a cell line, which the view collapses, are no part of the
// item: "-  a" in a cell reads as "- a" does.
func TestCellItemsTakeOneSpace(t *testing.T) {
	cell := func(md string) string {
		doc, _, err := FromMarkdown(context.Background(), "| x |\n| --- |\n| "+md+" |\n", nil)
		if err != nil {
			t.Fatal(err)
		}
		return key(doc.Content[0].Content[1])
	}
	for typed, want := range map[string]string{
		"-  a<br>-   b": "- a<br>- b", "1.  a<br>2.  b": "1. a<br>2. b",
		"a  -  b": "a  -  b", "`-  a`": "`-  a`", "*  a": "* a",
	} {
		if got, w := cell(typed), cell(want); got != w {
			t.Errorf("%q reads as %s, want %s", typed, got, w)
		}
	}
}

// TestLoneCRIsALineEnd proves CR and CRLF read as LF, so no CR is stored
// in text, where the view would show it as a space, and the edit of a
// document with CRLF in a code block keeps that block as it was.
func TestLoneCRIsALineEnd(t *testing.T) {
	for _, md := range []string{"a\rb\r\rc\r", "a\r\nb\r\n\r\nc\r\n", "a\nb\n\nc\n"} {
		doc, _, err := FromMarkdown(context.Background(), md, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := ToMarkdown(doc); got != "a b\n\nc\n" {
			t.Errorf("%q reads back as %q", md, got)
		}
		if k := key(doc); strings.Contains(k, "\r") || strings.Contains(k, `\r`) {
			t.Errorf("%q stores a CR: %s", md, k)
		}
	}
	code := Doc(&Node{Type: "codeBlock", Content: []*Node{{Type: "text", Text: "x\r\ny"}}},
		para("Intro"))
	md := strings.Replace(ToMarkdown(code), "Intro", "Intro edited", 1)
	out, ws, err := Edit(context.Background(), code, md, nil)
	if err != nil || len(ws) > 0 || out.Content[0] != code.Content[0] {
		t.Errorf("a code block with CRLF changed: %v %v %s", err, ws, key(out))
	}
}

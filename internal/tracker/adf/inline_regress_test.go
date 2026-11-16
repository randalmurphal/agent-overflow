package adf

import (
	"context"
	"slices"
	"strings"
	"testing"
)

func formattingLost(doc *Node) bool {
	for _, f := range lossy(doc) {
		if strings.Contains(f.phrase, "emphasis") || strings.Contains(f.phrase, "formatting") {
			return true
		}
	}
	return false
}

func shape(doc *Node) string {
	var types []string
	for n := doc; n != nil; {
		types = append(types, n.Type)
		if n.Type == "paragraph" || len(n.Content) == 0 {
			break
		}
		n = n.Content[0]
	}
	return strings.Join(types, ">")
}

func TestInlineDelimiterRegressions(t *testing.T) {
	s, e, k := Mark{Type: "strong"}, Mark{Type: "em"}, Mark{Type: "strike"}
	u := Mark{Type: "underline"}
	cases := []struct {
		name  string
		nodes []*Node
		md    string
		lossy bool
	}{
		{"space under two marks", []*Node{textNode("a", s), textNode(" ", s, e), textNode("b typo")},
			"**a** b typo\n", false},
		{"space under two marks in a tag", []*Node{textNode("a", s, u), textNode(" ", s, e, u), textNode("b", u)},
			"<u>**a** b</u>\n", false},
		{"tilde in strike", []*Node{textNode("x "), textNode("~", k), textNode(" y")},
			"x ~~&#126;~~ y\n", false},
		{"em on punctuation mid-word", []*Node{textNode("a"), textNode("_", e), textNode("b")},
			"a_b\n", true},
		{"strong on punctuation mid-word", []*Node{textNode("a"), textNode("*", s), textNode("b")},
			"a*b\n", true},
	}
	for _, tc := range cases {
		doc := Doc(&Node{Type: "paragraph", Content: tc.nodes})
		if got := ToMarkdown(doc); got != tc.md {
			t.Errorf("%s: ToMarkdown = %q, want %q", tc.name, got, tc.md)
		}
		if got := formattingLost(doc); got != tc.lossy {
			t.Errorf("%s: lossy = %v, want %v (%v)", tc.name, got, tc.lossy, lossy(doc))
		}
	}
}

// TestTaskBoxShapesStayText proves "[x]" text, and a link whose whole
// text is x, stay what they were at a list item start: goldmark reads
// "[x]" there as a task box whatever follows it.
func TestTaskBoxShapesStayText(t *testing.T) {
	link := Mark{Type: "link", Attrs: map[string]any{"href": "https://example.com"}}
	for _, tc := range []struct {
		nodes []*Node
		md    string
	}{
		{[]*Node{textNode("[x]foo")}, "- \\[x]foo\n"},
		{[]*Node{textNode("[ ]foo")}, "- \\[ ]foo\n"},
		{[]*Node{textNode("[X]")}, "- \\[X]\n"},
		{[]*Node{textNode("x", link), textNode(" ok")}, "- [&#120;](https://example.com) ok\n"},
		{[]*Node{textNode("X", link)}, "- [&#88;](https://example.com)\n"},
	} {
		doc := Doc(&Node{Type: "bulletList", Content: []*Node{{Type: "listItem",
			Content: []*Node{{Type: "paragraph", Content: tc.nodes}}}}})
		md := ToMarkdown(doc)
		if md != tc.md {
			t.Errorf("ToMarkdown = %q, want %q", md, tc.md)
		}
		back, _, err := FromMarkdown(context.Background(), md, nil)
		if err != nil || shape(back) != shape(doc) || mustJSON(back.Content[0].Content[0]) !=
			mustJSON(doc.Content[0].Content[0]) {
			t.Errorf("%q read back as %s %v", md, mustJSON(back), err)
		}
	}
}

// TestMentionWithoutIDRendersAsText proves a mention with an empty ID is
// written as the text it showed, which parses, and reported as lost.
func TestMentionWithoutIDRendersAsText(t *testing.T) {
	for text, want := range map[string]string{"@User 5": "@User 5\n", "": "@\n"} {
		doc := Doc(&Node{Type: "paragraph", Content: []*Node{{Type: "mention",
			Attrs: map[string]any{"id": "", "text": text}}}})
		md := ToMarkdown(doc)
		if md != want {
			t.Errorf("%q: ToMarkdown = %q, want %q", text, md, want)
		}
		if _, _, err := FromMarkdown(context.Background(), md, nil); err != nil {
			t.Errorf("%q: %v", text, err)
		}
		if !slices.Contains(warnFeatures(doc), "a mention with no account") {
			t.Errorf("%q: not reported lossy: %v", text, lossy(doc))
		}
	}
}

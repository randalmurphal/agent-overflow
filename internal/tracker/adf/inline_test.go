package adf

import (
	"context"
	"strings"
	"testing"
)

func TestTagShapedTextReadsWithoutBackslashes(t *testing.T) {
	for text, want := range map[string]string{
		"Returns List<String> and Map<K, V>": "Returns List<String> and Map<K, V>",
		"keep <u>this</u> literal":           `keep \<u>this\</u> literal`,
		"not a link <https://example.com/x>": `not a link \<https://example.com/x>`,
		"mail <a@example.com>":               `mail \<a@example.com>`,
		"a <!-- comment -->":                 `a \<!-- comment -->`,
	} {
		doc := &Node{Type: "doc", Content: []*Node{{Type: "paragraph",
			Content: []*Node{{Type: "text", Text: text}}}}}
		md := strings.TrimSpace(ToMarkdown(doc))
		if md != want {
			t.Errorf("ToMarkdown(%q) = %q, want %q", text, md, want)
		}
		back, _, err := FromMarkdown(context.Background(), md, nil)
		if err != nil || strings.TrimSpace(PlainText(back)) != text {
			t.Errorf("round trip of %q: %v %q", text, err, PlainText(back))
		}
	}
}

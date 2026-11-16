package adf

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func bigCodePage() *Node {
	var code strings.Builder
	for code.Len() < 500_000 {
		code.WriteString(`if (a[i] < b && c > d) { x = <y> & z; m["k"] = "v!"; } // see <http://example.com>` + "\n")
	}
	page := Doc()
	for i := range 9000 {
		page.Content = append(page.Content, para(fmt.Sprintf("Line %d of the notes: a, b & c [d].", i)))
		if i == 4500 {
			page.Content = append(page.Content, &Node{Type: "codeBlock",
				Attrs: map[string]any{"language": "c"}, Content: []*Node{{Type: "text", Text: code.String()}}})
		}
	}
	return page
}

func TestLongLookaheadsReadBack(t *testing.T) {
	long := strings.Repeat("ab ", 3000)
	for _, s := range []string{
		"[" + long + "](https://example.com)", "[" + long + "][r]", "<" + long + "x@example.com>",
		"<https://" + strings.Repeat("a", 2000) + ">", "&" + long + "amp;", "[a](b) " + long + "[",
	} {
		doc := Doc(para(s))
		md := ToMarkdown(doc)
		back, _, err := FromMarkdown(context.Background(), md, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := ToMarkdown(back); got != md || len(back.Content) != 1 ||
			len(back.Content[0].Content) != 1 || back.Content[0].Content[0].Text != s {
			t.Errorf("%.40q... reads back otherwise", s)
		}
	}
	// A one-sentence edit beside a 500 KB code block took 40s when every
	// changed-block check escaped the code as text; it takes well under 1s
	// (BenchmarkEditBigCodePage), and this bound holds under -race too.
	page := bigCodePage()
	md := strings.Replace(ToMarkdown(page), "Line 20 of", "Line 20 of the revised", 1)
	start := time.Now()
	if _, _, err := Edit(context.Background(), page, md, nil); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 20*time.Second {
		t.Errorf("a one-sentence edit beside a 500 KB code block took %v", d)
	}
}

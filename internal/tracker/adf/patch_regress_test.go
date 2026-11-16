package adf

import (
	"context"
	"testing"
)

func TestLozengeReadsBackAsTyped(t *testing.T) {
	doc := Doc(&Node{Type: "paragraph", Content: []*Node{{Type: "text", Text: "Build is "},
		{Type: "status", Attrs: map[string]any{"text": "DONE", "color": "green", "localId": "s1"}},
		{Type: "text", Text: " today."}}})
	for _, typed := range []string{"[In review]", "[in review]", "[IN REVIEW]", "[Mr NE]"} {
		md := "Build is " + typed + " today.\n"
		out, ws, err := Edit(context.Background(), doc, md, resolverFor(doc))
		if err != nil || len(ws) > 0 {
			t.Fatalf("%s: %v %v", typed, err, ws)
		}
		if got := ToMarkdown(out); got != md {
			t.Errorf("%s: the view reads %q", typed, got)
		}
		if s := out.Content[0].Content[1]; s.Type != "status" || s.attrStr("color") != "green" {
			t.Errorf("%s: the lozenge became %s", typed, s.Type)
		}
	}
}

package adf

import (
	"slices"
	"strings"
	"testing"
)

// A label names what Markdown cannot show with "the", so it drops the
// article a node's phrase carries.
func TestInventoryLabelsDropThePhrasesArticle(t *testing.T) {
	t.Parallel()
	doc := Doc(&Node{Type: "mediaSingle", Attrs: map[string]any{"layout": "center"}, Content: []*Node{
		{Type: "media", Attrs: map[string]any{"type": "external", "url": "https://example.com/a.png"}},
		{Type: "caption", Content: []*Node{{Type: "text", Text: "Figure 1"}}},
	}})
	var labels []string
	for _, r := range inventory(doc) {
		labels = append(labels, r.label)
		if strings.HasPrefix(r.label, "the a ") || strings.HasPrefix(r.label, "the an ") {
			t.Errorf("label %q", r.label)
		}
	}
	if !slices.Contains(labels, `the image caption of "Figure 1"`) {
		t.Fatalf("no caption label in %q", labels)
	}
}

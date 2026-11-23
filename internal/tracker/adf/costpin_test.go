//go:build !race

// The race detector drops pooled objects at random, so allocation counts
// are pinned only without it.

package adf

import (
	"context"
	"strings"
	"testing"
)

// The allocations of rendering a 17 KB description and of Edit of one
// sentence in it. Edit's vary by a few with map order, so its bound sits
// just above what was measured.
const (
	toMarkdownAllocs = 2530 // measured 2,530
	editAllocs       = 7230 // measured 7,216 to 7,217
)

func TestConversionAllocations(t *testing.T) {
	_, doc := corpusDoc(t, "doc-24.json")
	md := ToMarkdown(doc)
	i := strings.Index(md, "\n\n")
	if i < 0 {
		t.Fatal("no paragraph break")
	}
	edited := md[:i] + " Edited." + md[i:]
	render := testing.AllocsPerRun(20, func() { ToMarkdown(doc) })
	edit := testing.AllocsPerRun(20, func() {
		if _, _, err := Edit(context.Background(), doc, edited, nil); err != nil {
			t.Fatal(err)
		}
	})
	if render > toMarkdownAllocs {
		t.Errorf("ToMarkdown allocates %v times, want at most %d", render, toMarkdownAllocs)
	}
	if edit > editAllocs {
		t.Errorf("an edit of one sentence allocates %v times, want at most %d", edit, editAllocs)
	}
}

package claudetui

import "testing"

// TestImagePlaceholderLabelMatchesFrontend pins the cross-language marker format.
// The composer builds the same label in frontend/src/lib/utils/imagePlaceholders.ts
// (IMAGE_PLACEHOLDER_PREFIX="[Image #" + index + IMAGE_PLACEHOLDER_SUFFIX="]"). If
// the two drift, the send split stops finding markers and inline placement
// silently regresses to appending every image at the end.
func TestImagePlaceholderLabelMatchesFrontend(t *testing.T) {
	for _, tc := range []struct {
		index int
		want  string
	}{
		{1, "[Image #1]"},
		{2, "[Image #2]"},
		{12, "[Image #12]"},
	} {
		if got := imagePlaceholderLabel(tc.index); got != tc.want {
			t.Errorf("imagePlaceholderLabel(%d) = %q, want %q", tc.index, got, tc.want)
		}
	}
}

// TestSplitContentByImageMarkers covers the marker→parts split that positions each
// image where the user dropped it: inline, at either end, alone, multiple, and the
// edge cases (absent marker, repeated literal label, markers out of position).
func TestSplitContentByImageMarkers(t *testing.T) {
	txt := func(s string) imagePart { return imagePart{text: s, imageIndex: -1} }
	img := func(i int) imagePart { return imagePart{imageIndex: i} }

	tests := []struct {
		name       string
		content    string
		imageCount int
		want       []imagePart
	}{
		{"text only, no images", "hello world", 0, []imagePart{txt("hello world")}},
		{"empty, no images", "", 0, nil},
		{"image inline in the middle", "look at [Image #1] this", 1,
			[]imagePart{txt("look at "), img(0), txt(" this")}},
		{"marker at the start", "[Image #1] caption", 1, []imagePart{img(0), txt(" caption")}},
		{"marker at the end", "caption [Image #1]", 1, []imagePart{txt("caption "), img(0)}},
		{"marker alone", "[Image #1]", 1, []imagePart{img(0)}},
		{"two images at their markers", "a [Image #1] b [Image #2] c", 2,
			[]imagePart{txt("a "), img(0), txt(" b "), img(1), txt(" c")}},
		{"adjacent markers, no text between", "[Image #1][Image #2]", 2,
			[]imagePart{img(0), img(1)}},
		{"missing marker appends after text", "no marker here", 1,
			[]imagePart{txt("no marker here"), img(0)}},
		{"one marker present, one missing", "here [Image #1]", 2,
			[]imagePart{txt("here "), img(0), img(1)}},
		// A literal "[Image #1]" the user typed as prose must not be claimed twice:
		// the first occurrence is attachment 1's marker; the rest stays text.
		{"repeated label, single attachment", "[Image #1] and again [Image #1]", 1,
			[]imagePart{img(0), txt(" and again [Image #1]")}},
		// Markers bind to attachments by their number, not their textual order, but
		// the parts come out in position order.
		{"markers in reverse position", "[Image #2] then [Image #1]", 2,
			[]imagePart{img(1), txt(" then "), img(0)}},
		// Multibyte UTF-8 around ASCII markers: the split uses byte offsets, which
		// stay on rune boundaries because the labels are ASCII — so the text runs
		// (emoji, accents, CJK) come through whole. This is the subtlest property of
		// the split; a future move to rune-indexing must not break it.
		{"multibyte text around markers", "café 😀 [Image #1] señ [Image #2] 終", 2,
			[]imagePart{txt("café 😀 "), img(0), txt(" señ "), img(1), txt(" 終")}},
		// Whitespace between two adjacent markers is a non-empty text run, so it is
		// kept (preserving the spacing the user typed between two inline images) —
		// distinct from the zero-char "adjacent markers" case above.
		{"whitespace-only run between markers", "[Image #1]   [Image #2]", 2,
			[]imagePart{img(0), txt("   "), img(1)}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assertParts(t, splitContentByImageMarkers(tc.content, tc.imageCount), tc.want)
		})
	}
}

func assertParts(t *testing.T, got, want []imagePart) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d parts %+v, want %d %+v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("part %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

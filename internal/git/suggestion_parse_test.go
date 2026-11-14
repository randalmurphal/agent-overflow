package git

import (
	"reflect"
	"testing"
)

// GitLab's from_content and to_content: empty is no lines; one trailing
// line ending ends the last line and any other is an empty line.
func TestSplitContentLines(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		text string
		want []string
	}{
		{"", []string{}},
		{"\n", []string{""}},
		{"\r\n", []string{""}},
		{"a", []string{"a"}},
		{"a\n", []string{"a"}},
		{"a\n\n", []string{"a", ""}},
		{"\n\n", []string{"", ""}},
		{"a\nb", []string{"a", "b"}},
		{"a\r\nb\r\n", []string{"a", "b"}},
	} {
		if got := splitContentLines(tc.text); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("splitContentLines(%q) = %q, want %q", tc.text, got, tc.want)
		}
	}
}

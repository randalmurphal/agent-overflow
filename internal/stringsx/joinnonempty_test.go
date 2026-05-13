package stringsx

import "testing"

func TestJoinNonEmpty(t *testing.T) {
	cases := []struct {
		name  string
		sep   string
		parts []string
		want  string
	}{
		{"all empty", "\n\n", []string{"", "", ""}, ""},
		{"single value", "\n\n", []string{"only"}, "only"},
		{"trims whitespace", "\n\n", []string{"  a  ", "b"}, "a\n\nb"},
		{"skips blank-only entries", "\n\n", []string{"a", "   ", "b"}, "a\n\nb"},
		{"different separator", "; ", []string{"x", "y"}, "x; y"},
		{"no parts", "\n\n", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := JoinNonEmpty(tc.sep, tc.parts...); got != tc.want {
				t.Errorf("JoinNonEmpty(%q, %v) = %q, want %q", tc.sep, tc.parts, got, tc.want)
			}
		})
	}
}

package stringsx

import "testing"

func TestFirstNonEmpty(t *testing.T) {
	cases := []struct {
		name  string
		input []string
		want  string
	}{
		{"empty args", nil, ""},
		{"all empty", []string{"", "", ""}, ""},
		{"first wins", []string{"a", "b"}, "a"},
		{"skip empty", []string{"", "b", "c"}, "b"},
		{"whitespace is not empty", []string{" ", "x"}, " "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := FirstNonEmpty(tc.input...); got != tc.want {
				t.Fatalf("FirstNonEmpty(%v) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestFirstNonEmptyTrimmed(t *testing.T) {
	cases := []struct {
		name  string
		input []string
		want  string
	}{
		{"empty args", nil, ""},
		{"all empty", []string{"", "", ""}, ""},
		{"first wins", []string{"a", "b"}, "a"},
		{"whitespace treated as empty", []string{"   ", "x"}, "x"},
		{"returns trimmed", []string{"  hello  ", "x"}, "hello"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := FirstNonEmptyTrimmed(tc.input...); got != tc.want {
				t.Fatalf("FirstNonEmptyTrimmed(%v) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

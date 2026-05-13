package stringsx

import "testing"

func TestClip(t *testing.T) {
	cases := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{"shorter than max", "hello", 10, "hello"},
		{"equal to max", "hello", 5, "hello"},
		{"longer than max truncates", "hello world", 5, "hello"},
		{"empty string", "", 5, ""},
		{"zero max", "hello", 0, ""},
		{"negative max", "hello", -1, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Clip(tc.in, tc.max); got != tc.want {
				t.Errorf("Clip(%q, %d) = %q, want %q", tc.in, tc.max, got, tc.want)
			}
		})
	}
}

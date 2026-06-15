package stringsx

import "testing"

// TestSkipANSIEscape pins the escape-boundary contract both callers depend on:
// each escape kind is consumed to exactly its terminator, and an unterminated
// CSI/OSC resumes just past the ESC instead of swallowing the rest of the input.
// Every case is checked on BOTH the []byte and string instantiations — the
// generic must treat them identically.
func TestSkipANSIEscape(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		at, want int
	}{
		{"CSI SGR", "\x1b[31mX", 0, 5},
		{"CSI cursor move", "\x1b[2KX", 0, 4},
		{"CSI no params", "\x1b[mX", 0, 3},
		{"OSC BEL-terminated", "\x1b]0;t\x07X", 0, 6},
		{"OSC ST-terminated", "\x1b]0;t\x1b\\X", 0, 7},
		{"charset designator (", "\x1b(BX", 0, 3},
		{"charset designator )", "\x1b)0X", 0, 3},
		{"bare ESC consumes one byte", "\x1bMX", 0, 2},
		{"unterminated CSI resumes past ESC", "\x1b[1;2", 0, 1},
		{"unterminated OSC resumes past ESC", "\x1b]9;no terminator", 0, 1},
		{"lone trailing ESC", "ab\x1b", 2, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := SkipANSIEscape([]byte(tc.in), tc.at); got != tc.want {
				t.Errorf("SkipANSIEscape([]byte %q, %d) = %d, want %d", tc.in, tc.at, got, tc.want)
			}
			if got := SkipANSIEscape(tc.in, tc.at); got != tc.want {
				t.Errorf("SkipANSIEscape(string %q, %d) = %d, want %d", tc.in, tc.at, got, tc.want)
			}
		})
	}
}

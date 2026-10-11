package cilog

import (
	"strings"
	"testing"
)

// Escape sequences go, whole or split across writes at any byte; text
// that only looks like the start of one stays.
func TestPlainWriterDropsEscapeSequences(t *testing.T) {
	t.Parallel()
	in := "\x1b[36;1mExecuting\x1b[0;m \x1b[0Kdone\n\x1b[31mFAIL\x1b[m [x] \x1b] \x1b[12" + strings.Repeat("3", escapeMax) + "z end\x1b["
	want := "Executing done\nFAIL [x] \x1b] \x1b[12" + strings.Repeat("3", escapeMax) + "z end\x1b["
	for split := 0; split <= len(in); split++ {
		var out strings.Builder
		p := NewPlainWriter(&out)
		for _, part := range []string{in[:split], in[split:]} {
			if n, err := p.Write([]byte(part)); err != nil || n != len(part) {
				t.Fatalf("Write = %d, %v", n, err)
			}
		}
		if err := p.Close(); err != nil {
			t.Fatal(err)
		}
		if out.String() != want {
			t.Fatalf("split at %d: %q, want %q", split, out.String(), want)
		}
	}
}

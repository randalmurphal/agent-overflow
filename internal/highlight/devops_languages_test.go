package highlight

import (
	"fmt"
	"strings"
	"testing"
)

// These fixtures exercise whole-document state and UTF-8 span alignment in
// both code fences and reconstructed old/new patch sides.
func TestDevOpsLanguageSpans(t *testing.T) {
	type token struct {
		line  int
		text  string
		class uint16
	}
	cases := []struct {
		lang   Lang
		source string
		tokens []token
	}{
		{LangDockerfile, "# image café\nFROM alpine:3.20\nRUN echo first \\\n    && echo second\nENV GREETING=\"hello café\"", []token{
			{0, "café", ClassComment}, {1, "FROM", ClassKeyword}, {2, "RUN", ClassKeyword}, {4, "hello café", ClassString},
		}},
		{LangDockerfile, "FROM alpine\nRUN <<EOF\nprintf 'café'\nprintf 'done'\nEOF", []token{
			{0, "FROM", ClassKeyword}, {1, "RUN", ClassKeyword}, {2, "café", ClassString}, {3, "done", ClassString}, {4, "EOF", ClassKeyword},
		}},
		{LangMake, "# compile café\nCC := cc\nall: app\n\t$(CC) main.c \\\n\t  -o app", []token{
			{0, "café", ClassComment}, {1, "CC", ClassConstant}, {1, ":=", ClassOperator}, {2, "all", ClassConstant}, {3, "CC", ClassConstant},
		}},
		{LangXML, "<?xml version=\"1.0\"?>\n<!-- first\nsecond café -->\n<root enabled=\"true\"><item>café &amp; tea</item></root>", []token{
			{0, "xml", ClassKeyword}, {1, "first", ClassComment}, {2, "second café", ClassComment}, {3, "root", ClassTag}, {3, "enabled", ClassProperty}, {3, "true", ClassString}, {3, "&amp;", ClassConstant},
		}},
		{LangPowerShell, "$text = @\"\nhello café $env:USERNAME\nworld\n\"@\nWrite-Host $text", []token{
			{0, "$text", ClassVariable}, {1, "hello café", ClassString}, {1, "$env:USERNAME", ClassVariable}, {2, "world", ClassString}, {4, "Write-Host", ClassFunction},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.lang.String(), func(t *testing.T) {
			lines := strings.Split(tc.source, "\n")
			code := Highlight(tc.lang, []byte(tc.source))
			if code.Incomplete || code.Truncated || len(code.Lines) != len(lines) {
				t.Fatalf("code did not highlight completely: %+v", code)
			}
			patch := fmt.Sprintf("@@ -1,%d +1,%d @@\n-%s\n+%s\n", len(lines), len(lines), strings.Join(lines, "\n-"), strings.Join(lines, "\n+"))
			diff := HighlightPatchText(tc.lang, patch)
			if diff.Incomplete || diff.Truncated || len(diff.Lines) < 1+2*len(lines) {
				t.Fatalf("patch did not highlight completely: %+v", diff)
			}
			for i, line := range lines {
				expandRuns(t, code.Lines[i], len(line))
				expandRuns(t, diff.Lines[1+i], len(line))
				expandRuns(t, diff.Lines[1+len(lines)+i], len(line))
			}
			for _, want := range tc.tokens {
				for label, spans := range map[string]EncodedLine{
					"code": code.Lines[want.line], "deleted": diff.Lines[1+want.line], "added": diff.Lines[1+len(lines)+want.line],
				} {
					if got := classOf(t, spans, lines[want.line], want.text); got != want.class {
						t.Errorf("%s token %q class=%d, want %d", label, want.text, got, want.class)
					}
				}
			}
		})
	}
}

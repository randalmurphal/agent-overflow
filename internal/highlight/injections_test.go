package highlight

import (
	"strings"
	"testing"
)

const markdownFenceSample = "# Title\n" +
	"\n" +
	"Some prose with `inline code`.\n" +
	"\n" +
	"```python\n" +
	"def handler(request):\n" +
	"    \"\"\"Docstring with and, for keywords.\n" +
	"    Second docstring line.\n" +
	"    \"\"\"\n" +
	"    return request\n" +
	"```\n"

func TestMarkdownFenceInjection(t *testing.T) {
	res := Highlight(LangMarkdown, []byte(markdownFenceSample))
	lines := strings.Split(markdownFenceSample, "\n")

	if got := classOf(t, res.Lines[0], lines[0], "# Title"); got != ClassMarkupHeading {
		t.Errorf("heading class = %d, want markup-heading %d", got, ClassMarkupHeading)
	}
	// Injected python: `def` keyword on the fence's first code line.
	if got := classOf(t, res.Lines[5], lines[5], "def"); got != ClassKeyword {
		t.Errorf("injected `def` class = %d, want keyword %d", got, ClassKeyword)
	}
	// The docstring rule holds inside the injection: keywords in prose
	// stay string-classed across lines.
	for _, i := range []int{6, 7} {
		assertNoClass(t, res.Lines[i], lines[i], ClassKeyword, "injected docstring line")
	}
	if got := classOf(t, res.Lines[6], lines[6], "with and"); got != ClassString {
		t.Errorf("injected docstring class = %d, want string %d", got, ClassString)
	}
	if got := classOf(t, res.Lines[9], lines[9], "return"); got != ClassKeyword {
		t.Errorf("injected `return` class = %d, want keyword %d", got, ClassKeyword)
	}
}

const svelteSample = `<script lang="ts">
  let count: number = 0;
  function increment() {
    count += 1;
  }
</script>

<style>
  .button { color: red; }
</style>

<button onclick={increment}>clicked {count}</button>
`

func TestSvelteInjections(t *testing.T) {
	res := Highlight(LangSvelte, []byte(svelteSample))
	lines := strings.Split(svelteSample, "\n")

	// TypeScript injected via lang="ts".
	if got := classOf(t, res.Lines[1], lines[1], "let"); got != ClassKeyword {
		t.Errorf("injected `let` class = %d, want keyword %d", got, ClassKeyword)
	}
	if got := classOf(t, res.Lines[2], lines[2], "function"); got != ClassKeyword {
		t.Errorf("injected `function` class = %d, want keyword %d", got, ClassKeyword)
	}
	// CSS injected into <style>.
	if got := classOf(t, res.Lines[8], lines[8], "color"); got == ClassNone {
		t.Error("injected css property is unstyled")
	}
	// Host grammar still owns the tags.
	if got := classOf(t, res.Lines[0], lines[0], "script"); got != ClassTag {
		t.Errorf("script tag class = %d, want tag %d", got, ClassTag)
	}
}

const tsTemplateSample = "const query = `\n" +
	"  SELECT for while if\n" +
	"  FROM users\n" +
	"`;\n" +
	"const done = true;\n"

func TestTypeScriptMultilineTemplate(t *testing.T) {
	res := Highlight(LangTypeScript, []byte(tsTemplateSample))
	lines := strings.Split(tsTemplateSample, "\n")

	// Template-literal interior lines stay string-classed: the old
	// per-line tokenizer showed `for`/`while`/`if` as keywords here.
	for _, i := range []int{1, 2} {
		assertNoClass(t, res.Lines[i], lines[i], ClassKeyword, "template literal line")
	}
	if got := classOf(t, res.Lines[1], lines[1], "SELECT"); got != ClassString {
		t.Errorf("template line class = %d, want string %d", got, ClassString)
	}
	if got := classOf(t, res.Lines[4], lines[4], "const"); got != ClassKeyword {
		t.Errorf("`const` class = %d, want keyword %d", got, ClassKeyword)
	}
}

const htmlSample = `<html>
<style>
  body { margin: 0; }
</style>
<script>
  const x = 1;
</script>
</html>
`

func TestHTMLInjections(t *testing.T) {
	res := Highlight(LangHTML, []byte(htmlSample))
	lines := strings.Split(htmlSample, "\n")

	if got := classOf(t, res.Lines[0], lines[0], "html"); got != ClassTag {
		t.Errorf("html tag class = %d, want tag %d", got, ClassTag)
	}
	if got := classOf(t, res.Lines[5], lines[5], "const"); got != ClassKeyword {
		t.Errorf("injected js `const` class = %d, want keyword %d", got, ClassKeyword)
	}
	if got := classOf(t, res.Lines[2], lines[2], "margin"); got == ClassNone {
		t.Error("injected css property is unstyled")
	}
}

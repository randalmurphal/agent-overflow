package highlight

import (
	"strings"
	"testing"
)

type configToken struct {
	line  int
	text  string
	class uint16
}

var configLanguageSamples = []struct {
	lang   Lang
	source string
	tokens []configToken
}{
	{LangTOML, "# Café\n[package]\nname = \"café\"\nenabled = true\ncount = 3\nwhen = 2024-09-06T12:00:00Z\ndescription = \"\"\"\nfor café true\nstill string\n\"\"\"\nliteral = '''\nraw café text\n'''\n", []configToken{
		{0, "# Café", ClassComment}, {2, "name", ClassProperty}, {2, "café", ClassString}, {3, "true", ClassConstant}, {4, "3", ClassNumber}, {5, "2024-09-06T12:00:00Z", ClassStringSpecial}, {7, "for café true", ClassString}, {11, "raw café text", ClassString},
	}},
	{LangINI, "; Café settings\n[remote origin]\nurl = https://example.test/café\n", []configToken{
		{0, "; Café settings", ClassComment}, {1, "remote origin", ClassType}, {2, "url", ClassProperty},
	}},
	{LangHCL, "resource \"example\" \"café\" {\n  enabled = true\n  count = 3\n  text = <<EOF\nfor café true\nstill string\nEOF\n}\n", []configToken{
		{0, "café", ClassString}, {1, "enabled", ClassProperty}, {1, "true", ClassConstant}, {2, "3", ClassNumber}, {4, "for café true", ClassString}, {5, "still string", ClassString},
	}},
}

func TestConfigLanguageSpans(t *testing.T) {
	for _, sample := range configLanguageSamples {
		t.Run(sample.lang.String(), func(t *testing.T) {
			result := Highlight(sample.lang, []byte(sample.source))
			lines := strings.Split(sample.source, "\n")
			if result.Truncated || result.Incomplete || len(result.Lines) != len(lines) {
				t.Fatalf("unexpected degradation %#v", result)
			}
			for i, line := range lines {
				expandRuns(t, result.Lines[i], len(line))
			}
			for _, token := range sample.tokens {
				if got := classOf(t, result.Lines[token.line], lines[token.line], token.text); got != token.class {
					t.Errorf("%q class=%d want=%d", token.text, got, token.class)
				}
			}
		})
	}
}

func TestConfigLanguageContextualPatches(t *testing.T) {
	for _, sample := range []struct {
		lang          Lang
		source, patch string
		line          int
		text          string
		class         uint16
	}{
		{LangTOML, "description = \"\"\"\nopening\nnew café text\nclosing\n\"\"\"\n", "@@ -3 +3 @@\n-old text\n+new café text\n", 2, "new café text", ClassString},
		{LangHCL, "locals {\n  text = <<EOF\nopening\nnew café text\nclosing\nEOF\n}\n", "@@ -4 +4 @@\n-old text\n+new café text\n", 2, "new café text", ClassString},
		{LangINI, "[settings]\nname = café\n", "@@ -2 +2 @@\n-old = plain\n+name = café\n", 2, "name", ClassProperty},
	} {
		t.Run(sample.lang.String(), func(t *testing.T) {
			result := HighlightPatchTextPrimed(sample.lang, sample.patch, sample.source)
			lines := strings.Split(strings.TrimSuffix(sample.patch, "\n"), "\n")
			if result.Truncated || result.Incomplete || len(result.Lines) != len(lines) {
				t.Fatalf("unexpected patch degradation %#v", result)
			}
			for i, line := range lines {
				if i > 0 {
					line = line[1:]
				}
				expandRuns(t, result.Lines[i], len(line))
			}
			body := lines[sample.line][1:]
			if got := classOf(t, result.Lines[sample.line], body, sample.text); got != sample.class {
				t.Errorf("%q class=%d want=%d", sample.text, got, sample.class)
			}
		})
	}
}

func TestNewLanguageMalformedAndCappedInputs(t *testing.T) {
	capped := []byte(strings.Repeat("x", maxInputBytes+1))
	for _, lang := range []Lang{LangTOML, LangINI, LangHCL, LangDockerfile, LangMake, LangXML, LangPowerShell} {
		t.Run(lang.String(), func(t *testing.T) {
			malformed := "[broken { \"café\nunterminated\n"
			result := Highlight(lang, []byte(malformed))
			lines := strings.Split(malformed, "\n")
			for i, line := range result.Lines {
				if i >= len(lines) {
					t.Fatal("spans exceed input")
				}
				expandRuns(t, line, len(lines[i]))
			}
			if result := Highlight(lang, capped); !result.Truncated {
				t.Fatal("input cap must apply before grammar parsing")
			}
		})
	}
}

func BenchmarkConfigLanguages(b *testing.B) {
	for _, sample := range configLanguageSamples {
		b.Run(sample.lang.String(), func(b *testing.B) {
			source := []byte(sample.source)
			Highlight(sample.lang, source)
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				Highlight(sample.lang, source)
			}
		})
	}
}

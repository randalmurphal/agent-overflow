package highlight

import "testing"

func TestLangFromPath(t *testing.T) {
	cases := map[string]Lang{
		"src/main.ts":         LangTypeScript,
		"src/App.tsx":         LangTSX,
		"a/b/util.mjs":        LangJavaScript,
		"component.jsx":       LangJavaScript,
		"config.jsonc":        LangJSON,
		"internal/app.go":     LangGo,
		"scripts/run.PY":      LangPython,
		"deploy.zsh":          LangBash,
		"styles/app.scss":     LangCSS,
		"index.htm":           LangHTML,
		"Widget.svelte":       LangSvelte,
		"README.md":           LangMarkdown,
		"ci.yml":              LangYAML,
		"fix.patch":           LangDiff,
		"schema.sql":          LangSQL,
		"lib.rs":              LangRust,
		"kernel.c":            LangC,
		"kernel.h":            LangC,
		"engine.cpp":          LangCPP,
		"engine.hpp":          LangCPP,
		"Main.java":           LangJava,
		"Makefile":            LangPlaintext,
		"noext":               LangPlaintext,
		"dir.with.dots/noext": LangPlaintext,
		"archive.tar.gz":      LangPlaintext,
		"weird.unknownext":    LangPlaintext,
	}
	for path, want := range cases {
		if got := LangFromPath(path); got != want {
			t.Errorf("LangFromPath(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestLangFromName(t *testing.T) {
	cases := map[string]Lang{
		"python":     LangPython,
		"py":         LangPython,
		"Python":     LangPython,
		" golang ":   LangGo,
		"typescript": LangTypeScript,
		"tsx":        LangTSX,
		"jsx":        LangJavaScript,
		"shell":      LangBash,
		"console":    LangBash,
		"c++":        LangCPP,
		"text":       LangPlaintext,
		"":           LangPlaintext,
		"brainfuck":  LangPlaintext,
	}
	for name, want := range cases {
		if got := LangFromName(name); got != want {
			t.Errorf("LangFromName(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestLangStringRoundTrip(t *testing.T) {
	for lang, name := range langNames {
		if lang == LangPlaintext {
			continue
		}
		if got := LangFromName(name); got != lang {
			t.Errorf("LangFromName(%q) = %v, want %v (canonical names must round-trip)", name, got, lang)
		}
	}
}

func TestClassNamesTable(t *testing.T) {
	names := ClassNames()
	if len(names) != int(classCount) {
		t.Fatalf("ClassNames() length = %d, want %d", len(names), classCount)
	}
	seen := map[string]bool{}
	for id, name := range names {
		if name == "" {
			t.Errorf("class id %d has no name", id)
		}
		if seen[name] {
			t.Errorf("duplicate class name %q", name)
		}
		seen[name] = true
	}
	if names[ClassNone] != "none" {
		t.Errorf("class 0 must be \"none\", got %q", names[ClassNone])
	}
}

func TestCaptureFamily(t *testing.T) {
	cases := map[string]uint16{
		"keyword":                ClassKeyword,
		"keyword.control.import": ClassKeyword,
		"string":                 ClassString,
		"string.special.url":     ClassStringSpecial,
		"comment.documentation":  ClassComment,
		"function.builtin":       ClassFunction,
		"function.method.call":   ClassFunction,
		"constructor":            ClassFunction,
		"type.builtin":           ClassType,
		"variable.builtin":       ClassVariableBuiltin,
		"variable.parameter":     ClassParameter,
		"variable.other.member":  ClassProperty,
		"variable":               ClassVariable,
		"constant.builtin":       ClassConstant,
		"punctuation.bracket":    ClassPunctuation,
		"tag.attribute":          ClassAttribute,
		"tag.delimiter":          ClassPunctuation,
		"tag":                    ClassTag,
		"markup.heading.1":       ClassMarkupHeading,
		"text.title":             ClassMarkupHeading,
		"diff.plus":              ClassAdded,
		"conditional":            ClassKeyword,
		"_helper":                ClassNone,
		"spell":                  ClassNone,
	}
	for name, want := range cases {
		got, ok := captureFamily(name)
		if !ok {
			t.Errorf("captureFamily(%q) not recognized", name)
			continue
		}
		if got != want {
			t.Errorf("captureFamily(%q) = %d, want %d", name, got, want)
		}
	}
	if _, ok := captureFamily("totally.made.up"); ok {
		t.Error("captureFamily should report unknown captures as not-ok")
	}
}

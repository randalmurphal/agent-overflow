package highlight

import "strings"

// Lang identifies a grammar this package can highlight with. The zero
// value is LangPlaintext: unknown inputs degrade to plain text, never
// to an error.
type Lang int

const (
	LangPlaintext Lang = iota
	LangTypeScript
	LangTSX
	LangJavaScript // also serves .jsx — tree-sitter-javascript parses JSX natively
	LangJSON
	LangGo
	LangPython
	LangBash
	LangCSS
	LangHTML
	LangSvelte
	LangMarkdown
	// LangMarkdownInline is internal-only: the injected inline half of
	// the split markdown grammar. No extension or fence name maps to
	// it; markdown's injections query requests it by name.
	LangMarkdownInline
	LangYAML
	LangDiff
	LangSQL
	LangRust
	LangC
	LangCPP
	LangJava
)

// langNames is the canonical wire name per Lang (shiki-compatible so
// existing markdown fence names resolve unchanged).
var langNames = map[Lang]string{
	LangPlaintext:      "plaintext",
	LangTypeScript:     "typescript",
	LangTSX:            "tsx",
	LangJavaScript:     "javascript",
	LangJSON:           "json",
	LangGo:             "go",
	LangPython:         "python",
	LangBash:           "bash",
	LangCSS:            "css",
	LangHTML:           "html",
	LangSvelte:         "svelte",
	LangMarkdown:       "markdown",
	LangMarkdownInline: "markdown-inline",
	LangYAML:           "yaml",
	LangDiff:           "diff",
	LangSQL:            "sql",
	LangRust:           "rust",
	LangC:              "c",
	LangCPP:            "cpp",
	LangJava:           "java",
}

func (l Lang) String() string {
	if name, ok := langNames[l]; ok {
		return name
	}
	return "plaintext"
}

// extensionToLang mirrors the historical frontend map exactly
// (frontend/src/lib/utils/diffLanguage.ts before its removal), plus
// the C-family additions for chat code blocks. The backend is now the
// single owner of language detection.
var extensionToLang = map[string]Lang{
	"ts":       LangTypeScript,
	"mts":      LangTypeScript,
	"cts":      LangTypeScript,
	"tsx":      LangTSX,
	"js":       LangJavaScript,
	"jsx":      LangJavaScript,
	"mjs":      LangJavaScript,
	"cjs":      LangJavaScript,
	"json":     LangJSON,
	"jsonc":    LangJSON,
	"go":       LangGo,
	"py":       LangPython,
	"pyi":      LangPython,
	"sh":       LangBash,
	"bash":     LangBash,
	"zsh":      LangBash,
	"css":      LangCSS,
	"scss":     LangCSS,
	"html":     LangHTML,
	"htm":      LangHTML,
	"svelte":   LangSvelte,
	"md":       LangMarkdown,
	"mdx":      LangMarkdown,
	"markdown": LangMarkdown,
	"yaml":     LangYAML,
	"yml":      LangYAML,
	"diff":     LangDiff,
	"patch":    LangDiff,
	"sql":      LangSQL,
	"rs":       LangRust,
	"c":        LangC,
	"h":        LangC,
	"cpp":      LangCPP,
	"cc":       LangCPP,
	"cxx":      LangCPP,
	"hpp":      LangCPP,
	"hh":       LangCPP,
	"java":     LangJava,
}

// nameAliases resolves markdown fence info strings beyond the
// canonical names (agents write ```py, ```shell, ```golang, …).
var nameAliases = map[string]Lang{
	"py":              LangPython,
	"python":          LangPython,
	"golang":          LangGo,
	"ts":              LangTypeScript,
	"js":              LangJavaScript,
	"jsx":             LangJavaScript,
	"sh":              LangBash,
	"shell":           LangBash,
	"zsh":             LangBash,
	"console":         LangBash,
	"scss":            LangCSS,
	"md":              LangMarkdown,
	"mdx":             LangMarkdown,
	"yml":             LangYAML,
	"markdown_inline": LangMarkdownInline,
	"markdown.inline": LangMarkdownInline, // helix injection queries use this spelling
	"patch":           LangDiff,
	"rs":              LangRust,
	"c++":             LangCPP,
	"cc":              LangCPP,
	"text":            LangPlaintext,
	"txt":             LangPlaintext,
}

// LangFromPath maps a file path to a Lang by extension. Unknown or
// extension-less paths return LangPlaintext.
func LangFromPath(path string) Lang {
	slash := strings.LastIndexByte(path, '/')
	filename := path[slash+1:]
	dot := strings.LastIndexByte(filename, '.')
	if dot < 0 {
		return LangPlaintext
	}
	ext := strings.ToLower(filename[dot+1:])
	if lang, ok := extensionToLang[ext]; ok {
		return lang
	}
	return LangPlaintext
}

// LangFromName maps a language name (markdown fence info string or
// canonical name) to a Lang. Unknown names return LangPlaintext.
func LangFromName(name string) Lang {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return LangPlaintext
	}
	for lang, canonical := range langNames {
		if name == canonical {
			return lang
		}
	}
	if lang, ok := nameAliases[name]; ok {
		return lang
	}
	return LangPlaintext
}

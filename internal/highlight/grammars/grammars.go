// Package grammars binds tree-sitter grammar modules to their vendored
// editor-grade highlight queries. Each language directory holds the
// query files (embedded), the query source's LICENSE, and an UPSTREAM
// pin recording grammar version, query revision, and any hand-applied
// reconciliations.
//
// Grammar C code comes from upstream Go modules where one exists;
// languages without a Go binding (sql, markdown, diff) vendor their
// generated parser source under their directory with a local cgo
// binding.
package grammars

import (
	"embed"
	"fmt"
	"hash/fnv"
	"io/fs"
	"sync"

	tree_sitter_yaml "github.com/tree-sitter-grammars/tree-sitter-yaml/bindings/go"
	tree_sitter "github.com/tree-sitter/go-tree-sitter"
	tree_sitter_bash "github.com/tree-sitter/tree-sitter-bash/bindings/go"
	tree_sitter_c "github.com/tree-sitter/tree-sitter-c/bindings/go"
	tree_sitter_cpp "github.com/tree-sitter/tree-sitter-cpp/bindings/go"
	tree_sitter_css "github.com/tree-sitter/tree-sitter-css/bindings/go"
	tree_sitter_go "github.com/tree-sitter/tree-sitter-go/bindings/go"
	tree_sitter_html "github.com/tree-sitter/tree-sitter-html/bindings/go"
	tree_sitter_java "github.com/tree-sitter/tree-sitter-java/bindings/go"
	tree_sitter_javascript "github.com/tree-sitter/tree-sitter-javascript/bindings/go"
	tree_sitter_json "github.com/tree-sitter/tree-sitter-json/bindings/go"
	tree_sitter_python "github.com/tree-sitter/tree-sitter-python/bindings/go"
	tree_sitter_rust "github.com/tree-sitter/tree-sitter-rust/bindings/go"
	tree_sitter_typescript "github.com/tree-sitter/tree-sitter-typescript/bindings/go"

	tree_sitter_sql "github.com/DerekStride/tree-sitter-sql/bindings/go"

	diff_grammar "agent-overflow/internal/highlight/grammars/diff"
	markdown_grammar "agent-overflow/internal/highlight/grammars/markdown"
	markdowninline_grammar "agent-overflow/internal/highlight/grammars/markdown-inline"
	svelte_grammar "agent-overflow/internal/highlight/grammars/svelte"
)

//go:embed */highlights.scm */injections.scm */UPSTREAM
var queryFS embed.FS

// Grammar bundles everything the highlight engine needs for one
// language. Language is the shared static grammar object (immutable,
// safe for concurrent use); the query strings are raw .scm sources
// compiled by the caller.
type Grammar struct {
	Language   *tree_sitter.Language
	Highlights string
	Injections string // empty when the language has no injections query
}

// specs is keyed by the canonical language name (highlight.Lang's
// String form). Adding a language = module import (or vendored
// binding) + query files in <name>/ + one row.
var specs = map[string]func() *tree_sitter.Language{
	"python": func() *tree_sitter.Language { return tree_sitter.NewLanguage(tree_sitter_python.Language()) },
	"go":     func() *tree_sitter.Language { return tree_sitter.NewLanguage(tree_sitter_go.Language()) },
	"typescript": func() *tree_sitter.Language {
		return tree_sitter.NewLanguage(tree_sitter_typescript.LanguageTypescript())
	},
	"tsx":        func() *tree_sitter.Language { return tree_sitter.NewLanguage(tree_sitter_typescript.LanguageTSX()) },
	"javascript": func() *tree_sitter.Language { return tree_sitter.NewLanguage(tree_sitter_javascript.Language()) },
	"json":       func() *tree_sitter.Language { return tree_sitter.NewLanguage(tree_sitter_json.Language()) },
	"yaml":       func() *tree_sitter.Language { return tree_sitter.NewLanguage(tree_sitter_yaml.Language()) },
	"bash":       func() *tree_sitter.Language { return tree_sitter.NewLanguage(tree_sitter_bash.Language()) },
	"css":        func() *tree_sitter.Language { return tree_sitter.NewLanguage(tree_sitter_css.Language()) },
	"html":       func() *tree_sitter.Language { return tree_sitter.NewLanguage(tree_sitter_html.Language()) },
	"svelte":     func() *tree_sitter.Language { return tree_sitter.NewLanguage(svelte_grammar.Language()) },
	"rust":       func() *tree_sitter.Language { return tree_sitter.NewLanguage(tree_sitter_rust.Language()) },
	"c":          func() *tree_sitter.Language { return tree_sitter.NewLanguage(tree_sitter_c.Language()) },
	"cpp":        func() *tree_sitter.Language { return tree_sitter.NewLanguage(tree_sitter_cpp.Language()) },
	"java":       func() *tree_sitter.Language { return tree_sitter.NewLanguage(tree_sitter_java.Language()) },
	"sql":        func() *tree_sitter.Language { return tree_sitter.NewLanguage(tree_sitter_sql.Language()) },
	"markdown":   func() *tree_sitter.Language { return tree_sitter.NewLanguage(markdown_grammar.Language()) },
	"markdown-inline": func() *tree_sitter.Language {
		return tree_sitter.NewLanguage(markdowninline_grammar.Language())
	},
	"diff": func() *tree_sitter.Language { return tree_sitter.NewLanguage(diff_grammar.Language()) },
}

var (
	mu    sync.Mutex
	built = map[string]*Grammar{}
)

// Get returns the grammar bundle for a canonical language name, or
// ok=false when no grammar is wired up (callers degrade to plain
// text). The bundle is built once and shared.
func Get(name string) (*Grammar, bool) {
	mu.Lock()
	defer mu.Unlock()
	if g, ok := built[name]; ok {
		return g, true
	}
	language, ok := specs[name]
	if !ok {
		return nil, false
	}
	g := &Grammar{
		Language:   language(),
		Highlights: query(name, "highlights"),
		Injections: query(name, "injections"),
	}
	built[name] = g
	return g, true
}

// query reads an embedded query file; languages without an injections
// query simply have no such file.
func query(name, kind string) string {
	data, err := queryFS.ReadFile(name + "/" + kind + ".scm")
	if err != nil {
		return ""
	}
	return string(data)
}

// SchemaDigest returns a deterministic FNV-1a digest over every
// embedded query and UPSTREAM pin file (path + contents, walked in
// lexical order). It changes when any vendored query or a grammar's
// recorded upstream revision changes — one input to
// highlight.SchemaVersion, which version-stamps persisted spans.
func SchemaDigest() uint64 {
	h := fnv.New64a()
	err := fs.WalkDir(queryFS, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		data, readErr := queryFS.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		h.Write([]byte(path))
		h.Write([]byte{0})
		h.Write(data)
		h.Write([]byte{0})
		return nil
	})
	if err != nil {
		// The FS is compile-time embedded data; a read failure is a
		// build corruption, not a runtime condition to degrade around.
		panic(fmt.Sprintf("grammars: walk embedded queries: %v", err))
	}
	return h.Sum64()
}

// Names returns every language name with a wired-up grammar, for
// test harnesses that must cover the full set.
func Names() []string {
	names := make([]string, 0, len(specs))
	for name := range specs {
		names = append(names, name)
	}
	return names
}

// ABI sanity: a grammar generated with a newer tree-sitter CLI than
// the runtime supports fails loudly instead of misbehaving.
func init() {
	for name, language := range specs {
		v := language().AbiVersion()
		if v < tree_sitter.MIN_COMPATIBLE_LANGUAGE_VERSION || v > tree_sitter.LANGUAGE_VERSION {
			panic(fmt.Sprintf("grammar %q ABI version %d outside supported range [%d, %d]",
				name, v, tree_sitter.MIN_COMPATIBLE_LANGUAGE_VERSION, tree_sitter.LANGUAGE_VERSION))
		}
	}
}

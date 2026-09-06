package highlight

import (
	"strings"
	"testing"

	tree_sitter "github.com/tree-sitter/go-tree-sitter"

	"agent-overflow/internal/highlight/grammars"
)

// The lockstep pin: every vendored query must compile against its
// pinned grammar, and every capture it defines must resolve in the
// class taxonomy. A grammar bump that breaks a query, or a query that
// introduces an unmapped capture, fails here instead of silently
// rendering plain.
func TestVendoredQueriesCompileAndMapCaptures(t *testing.T) {
	if err := Preflight(); err != nil {
		t.Fatal(err)
	}
	for _, name := range grammars.Names() {
		g, ok := grammars.Get(name)
		if !ok {
			t.Fatalf("grammar %q vanished", name)
		}
		for label, source := range map[string]string{"highlights": g.Highlights, "injections": g.Injections} {
			if source == "" {
				continue
			}
			q, err := tree_sitter.NewQuery(g.Language, source)
			if err != nil {
				t.Errorf("grammar %s: %s query does not compile: %v", name, label, err)
				continue
			}
			if label == "highlights" {
				for _, capture := range q.CaptureNames() {
					if strings.HasPrefix(capture, "_") {
						continue
					}
					if _, mapped := captureFamily(capture); !mapped {
						t.Errorf("grammar %s: capture @%s has no class mapping", name, capture)
					}
				}
			}
			q.Close()
		}
	}
}

// Every grammar and non-plaintext Lang must have a matching registry entry;
// adding a language without wiring its parser must fail CI.
func TestGrammarRegistryAlignment(t *testing.T) {
	for _, name := range grammars.Names() {
		if LangFromName(name) == LangPlaintext {
			t.Errorf("grammar %q has no Lang registry entry", name)
		}
	}
	for lang, name := range langNames {
		if lang == LangPlaintext {
			continue
		}
		if _, ok := grammars.Get(name); !ok {
			t.Errorf("language %q has no grammar", name)
			continue
		}
		res := Highlight(lang, []byte("sample text\n"))
		if len(res.Lines) != 2 {
			t.Errorf("Highlight(%s) returned %d lines, want 2", name, len(res.Lines))
		}
	}
}

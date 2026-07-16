package highlight

import (
	"fmt"
	"sync"

	tree_sitter "github.com/tree-sitter/go-tree-sitter"

	"agent-overflow/internal/highlight/grammars"
)

// engine is one language's compiled highlighting machinery: the shared
// grammar, the highlights query compiled once, and the capture-index →
// class-id table precomputed so the hot path never touches strings.
// All fields are immutable after build; engines are shared across
// goroutines (queries are read-only during matching, cursors and
// parsers are per-call).
type engine struct {
	lang         *tree_sitter.Language
	query        *tree_sitter.Query
	captureClass []uint16        // indexed by query capture index
	inj          *injectionQuery // nil when the language has no injections
}

var engineTable = struct {
	mu    sync.Mutex
	built map[Lang]*engine
	errs  map[Lang]error
}{built: map[Lang]*engine{}, errs: map[Lang]error{}}

// engineFor returns the engine for a language, or nil when the
// language has no grammar wired up or its vendored query failed to
// compile — both degrade to plain text. Compile failures are build
// defects; Preflight surfaces them loudly (tests fail CI, the app can
// log at startup).
func engineFor(lang Lang) *engine {
	engineTable.mu.Lock()
	defer engineTable.mu.Unlock()
	if e, ok := engineTable.built[lang]; ok {
		return e
	}
	if _, failed := engineTable.errs[lang]; failed {
		return nil
	}
	e, err := buildEngine(lang)
	if err != nil {
		engineTable.errs[lang] = err
		return nil
	}
	engineTable.built[lang] = e // nil for grammarless languages
	return e
}

func buildEngine(lang Lang) (*engine, error) {
	g, ok := grammars.Get(lang.String())
	if !ok {
		return nil, nil
	}
	query, err := tree_sitter.NewQuery(g.Language, g.Highlights)
	if err != nil {
		return nil, fmt.Errorf("compile %s highlights query: %w", lang, err)
	}
	names := query.CaptureNames()
	captureClass := make([]uint16, len(names))
	for i, name := range names {
		class, _ := captureFamily(name)
		captureClass[i] = class // unknown → ClassNone; harness fails CI on those
	}
	inj, injErr := compileInjections(g.Language, g.Injections)
	if injErr != nil {
		query.Close()
		return nil, fmt.Errorf("compile %s injections query: %w", lang, injErr)
	}
	return &engine{
		lang:         g.Language,
		query:        query,
		captureClass: captureClass,
		inj:          inj,
	}, nil
}

// Preflight forces every wired-up grammar's engine build and returns
// the first failure. Tests gate on it; callers that want a loud
// startup check can too.
func Preflight() error {
	for _, name := range grammars.Names() {
		lang := LangFromName(name)
		if lang == LangPlaintext {
			return fmt.Errorf("grammar %q has no Lang registry entry", name)
		}
		engineFor(lang)
		engineTable.mu.Lock()
		err := engineTable.errs[lang]
		engineTable.mu.Unlock()
		if err != nil {
			return err
		}
	}
	return nil
}

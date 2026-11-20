package linear

import (
	"fmt"
	"strings"
)

// operation is one GraphQL document. newQuery and newMutation are the
// only ways to make one, and both register it, so the schema test checks
// every document the package can send; a test refuses any other
// operation literal in the package's source.
type operation struct {
	// name is the document's operation name, which the answer's errors
	// and the tests key on.
	name string
	text string
	// root is the response key of the field holding the resource the
	// call asks for, where a not-found error for that resource is
	// reported. Paged documents alias their connection as "page" under a
	// parent aliased "node"; mutations alias their root field "result".
	root string
	// mutation marks a write: it is never replayed and its answer is read
	// for its outcome. A mutation's document aliases its payload's entity
	// as "value".
	mutation bool
	// results are a mutation's response keys holding its payloads, each
	// of which must say success: true for the write to have applied.
	results []string
	// maxBody bounds the answer when the default 8 MiB is too small: a
	// page of issues carries their descriptions.
	maxBody int64
	// paged marks a document that declares $after: fetchPage refuses to
	// send it with variables that do not say which cursor they hold.
	paged bool
}

// registered holds every operation by name. It is written only while
// the package initializes its operation variables, and read by tests.
var registered = map[string]operation{}

// newQuery makes and registers a query whose resource is at root.
func newQuery(name, root, text string) operation {
	return register(operation{name: name, root: root, text: text, paged: strings.Contains(text, "$after")})
}

// newMutation makes and registers a mutation; its document aliases the
// root field "result".
func newMutation(name, text string) operation {
	return register(operation{name: name, root: "result", text: text, mutation: true, results: []string{"result"}})
}

func register(op operation) operation {
	if _, dup := registered[op.name]; dup {
		panic(fmt.Sprintf("linear: operation %s is declared twice", op.name))
	}
	registered[op.name] = op
	return op
}

// family is a mutation document made for n issues at once: the same
// operation name, with one root field per issue aliased i0 to i(n-1),
// each with variables of its own. Its text depends on n alone, never on
// a caller's values, and every document for n from 1 to max is made once
// when the package initializes, so the schema test checks each one
// (families).
type family struct {
	name string
	// docs holds the document for n issues at n-1.
	docs []operation
}

// families holds every family by name; its largest document is also in
// registered, so the checks by operation name cover it.
var families = map[string]family{}

// newMutationFamily makes and registers a family of mutations.
func newMutationFamily(name string, max int, text func(n int) string) family {
	f := family{name: name, docs: make([]operation, max)}
	for n := 1; n <= max; n++ {
		results := make([]string, n)
		for i := range results {
			results[i] = alias(i)
		}
		f.docs[n-1] = operation{name: name, text: text(n), mutation: true, results: results}
	}
	register(f.docs[max-1])
	families[name] = f
	return f
}

// op is the family's document for n issues, 1 to its max.
func (f family) op(n int) operation {
	if n < 1 || n > len(f.docs) {
		panic(fmt.Sprintf("linear: %s for %d issues, outside 1 to %d", f.name, n, len(f.docs)))
	}
	return f.docs[n-1]
}

// withMaxBody is op with a larger answer bound.
func (op operation) withMaxBody(n int64) operation {
	op.maxBody = n
	return op
}

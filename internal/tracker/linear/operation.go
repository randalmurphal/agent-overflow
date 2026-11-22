package linear

import (
	"fmt"
	"strings"
	"sync"
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

// Every operation and family the package declares is registered for its
// tests (the schema check, the exercise check), in fixed arrays written
// only while the package initializes its operation variables, so that
// declaring one allocates nothing when the package is linked into a
// process that never uses it. A name declared twice is a test failure.
const (
	maxOperations = 64
	maxFamilies   = 4
)

var registry struct {
	ops      [maxOperations]operation
	nOps     int
	families [maxFamilies]family
	nFam     int
}

// resultKeys are a single mutation's response keys, shared by every
// mutation so that declaring one allocates nothing. Never written.
var resultKeys = []string{"result"}

// newQuery makes and registers a query whose resource is at root.
func newQuery(name, root, text string) operation {
	return register(operation{name: name, root: root, text: text, paged: strings.Contains(text, "$after")})
}

// newMutation makes and registers a mutation; its document aliases the
// root field "result".
func newMutation(name, text string) operation {
	return register(operation{name: name, root: "result", text: text, mutation: true, results: resultKeys})
}

func register(op operation) operation {
	if registry.nOps == len(registry.ops) {
		panic(fmt.Sprintf("linear: operation %s is past maxOperations; raise it", op.name))
	}
	registry.ops[registry.nOps] = op
	registry.nOps++
	return op
}

// family is a mutation document made for n issues at once: the same
// operation name, with one root field per issue aliased i0 to i(n-1),
// each with variables of its own. Its text depends on n alone, never on
// a caller's values. The document for n is made the first time n is
// used, once, and kept; the schema test makes and checks every n.
type family struct {
	name string
	text func(n int) string
	// docs holds the document for n issues at n-1, its length the
	// family's max.
	docs []familyDoc
}

type familyDoc struct {
	once sync.Once
	op   operation
}

// newMutationFamily makes and registers a family of mutations whose
// documents are kept in docs, one per n from 1 to len(docs): storage the
// caller declares as a package array, so declaring the family allocates
// nothing.
func newMutationFamily(name string, docs []familyDoc, text func(n int) string) family {
	f := family{name: name, text: text, docs: docs}
	if registry.nFam == len(registry.families) {
		panic(fmt.Sprintf("linear: family %s is past maxFamilies; raise it", name))
	}
	registry.families[registry.nFam] = f
	registry.nFam++
	return f
}

// op is the family's document for n issues, 1 to its max, made on its
// first use. Safe for concurrent use; after the first use of n it costs
// one atomic load (sync.Once's).
func (f family) op(n int) operation {
	if n < 1 || n > len(f.docs) {
		panic(fmt.Sprintf("linear: %s for %d issues, outside 1 to %d", f.name, n, len(f.docs)))
	}
	d := &f.docs[n-1]
	d.once.Do(func() { d.op = f.member(n) })
	return d.op
}

// member makes the family's document for n issues.
func (f family) member(n int) operation {
	results := make([]string, n)
	for i := range results {
		results[i] = alias(i)
	}
	return operation{name: f.name, text: f.text(n), mutation: true, results: results}
}

// withMaxBody is op with a larger answer bound.
func (op operation) withMaxBody(n int64) operation {
	op.maxBody = n
	return op
}

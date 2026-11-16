package linear

import "fmt"

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
	// maxBody bounds the answer when the default 8 MiB is too small: a
	// page of issues carries their descriptions.
	maxBody int64
}

// registered holds every operation by name. It is written only while
// the package initializes its operation variables, and read by tests.
var registered = map[string]operation{}

// newQuery makes and registers a query whose resource is at root.
func newQuery(name, root, text string) operation {
	return register(operation{name: name, root: root, text: text})
}

// newMutation makes and registers a mutation; its document aliases the
// root field "result".
func newMutation(name, text string) operation {
	return register(operation{name: name, root: "result", text: text, mutation: true})
}

func register(op operation) operation {
	if _, dup := registered[op.name]; dup {
		panic(fmt.Sprintf("linear: operation %s is declared twice", op.name))
	}
	registered[op.name] = op
	return op
}

// withMaxBody is op with a larger answer bound.
func (op operation) withMaxBody(n int64) operation {
	op.maxBody = n
	return op
}

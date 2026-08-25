// Fixture file proving *_test.go declarations never reach the table.
// Under testdata/, so the go tool never builds or runs it.
package alpha

// TestOnlyAlpha must never be collected: bindings only consider
// production code.
func (a *Alpha) TestOnlyAlpha() {}

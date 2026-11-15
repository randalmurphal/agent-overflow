package transport

import "time"

// MethodBudget is how long the server side of the named method may run:
// its //ao:budget, else DefaultMethodBudget. A name the generated table
// does not carry (a harness method) has the default.
func MethodBudget(methodName string) time.Duration {
	if budget := classify(methodName).Budget; budget > 0 {
		return budget
	}
	return DefaultMethodBudget
}

// ClientWait is how long a client waits for the named method's answer: its
// budget and MethodAnswerMargin, so the server's answer, a timeout of its
// own included, arrives first. Every client derives its wait from here or
// from the generated frontend mirror (methodRoutes.ts), never from a number
// of its own (docs/architecture/transport.md#method-budgets).
func ClientWait(methodName string) time.Duration {
	return MethodBudget(methodName) + MethodAnswerMargin
}

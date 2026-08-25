package provider

// SingleUserInputAnswer constructs a single-answer response value.
//
// Test support only — production answer values arrive decoded off the wire
// (UserInputAnswer.UnmarshalJSON), never constructed here. It lives in a
// non-test file because the tests that build them sit in four packages
// (provider, provider/claude, provider/codex, triage) and a `_test.go`
// helper is not importable across a package boundary.
func SingleUserInputAnswer(value string) UserInputAnswer {
	return UserInputAnswer{value}
}

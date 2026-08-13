package codex

import (
	"errors"
	"fmt"
	"testing"
)

func TestIsThreadNotFoundRequiresTheResumeErrorShape(t *testing.T) {
	for _, message := range []string{
		"no rollout found for thread id 00000000-0000-4000-8000-000000000001",
		"thread not found: abc",
	} {
		match := fmt.Errorf("start session: %w", &RPCError{
			Method: "thread/resume", Code: -32600, Message: message,
		})
		if !IsThreadNotFound(match) {
			t.Fatalf("wrapped thread/resume not-found error %q was not recognized", message)
		}
	}
	for _, err := range []error{
		nil,
		errors.New("thread not found: abc"),
		&RPCError{Method: "thread/read", Code: -32600, Message: "thread not found: abc"},
		&RPCError{Method: "thread/resume", Code: -32603, Message: "thread not found: abc"},
		&RPCError{Method: "thread/resume", Code: -32600, Message: "invalid model"},
	} {
		if IsThreadNotFound(err) {
			t.Fatalf("unrelated error was recognized as thread loss: %v", err)
		}
	}
}

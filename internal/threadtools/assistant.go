package threadtools

import (
	"context"
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// thread_new_assistant: an assistant creates another assistant
// (docs/specs/agent-thread-tools.md#thread_new_assistant). The new one
// starts as a spawn of the caller whose prompt is the app-written opening
// block, so it answers, and routes its questions, to its creator.

type newAssistantArgs struct {
	Name string `json:"name"`
	Role string `json:"role"`
}

// NewAssistantCall creates an assistant on the caller's computer.
type NewAssistantCall struct {
	Name string `json:"name"`
	// Role is the creator's opening role text, quoted to the new assistant
	// as the creator's words.
	Role string `json:"role"`
}

// NewAssistantAck is the spawn's receipt with the assistant it created.
type NewAssistantAck struct {
	RequestAck
	AssistantID string `json:"assistant_id"`
	Name        string `json:"name"`
	Home        string `json:"home"`
}

type newAssistantResult struct {
	ackResult
	AssistantID string `json:"assistant_id"`
	Name        string `json:"name"`
	Home        string `json:"home"`
}

func (c *session) newAssistant(ctx context.Context, raw json.RawMessage) (any, error) {
	var args newAssistantArgs
	if err := decode(raw, &args); err != nil {
		return nil, err
	}
	if Forwarded(ctx) {
		return nil, invalidf("thread_new_assistant creates an assistant on the caller's own computer and is never forwarded.")
	}
	call := NewAssistantCall{Name: trim(args.Name), Role: trim(args.Role)}
	if call.Name == "" {
		return nil, invalidf("name is required and cannot be blank.")
	}
	if strings.ContainsAny(call.Name, "\r\n") {
		return nil, invalidf("name must be one line: it is the assistant's thread title.")
	}
	if utf8.RuneCountInString(call.Name) > MaxTitleRunes {
		return nil, invalidf("name must be at most %d characters.", MaxTitleRunes)
	}
	if err := checkText(call.Role, "role"); err != nil {
		return nil, err
	}
	ack, err := c.app.NewAssistant(ctx, c.caller, call)
	if err != nil {
		return nil, err
	}
	return newAssistantResult{
		ackResult:   c.ack(ack.RequestAck, "spawn"),
		AssistantID: ack.AssistantID,
		Name:        ack.Name,
		Home:        ack.Home,
	}, nil
}

func newAssistantSchema() map[string]any {
	properties := map[string]any{
		"name": map[string]any{
			"type":        "string",
			"description": "The new assistant's name, which is its thread title. Names are unique on this computer, ignoring case; a name in use is refused naming the assistant that holds it.",
		},
		"role": map[string]any{
			"type":        "string",
			"description": "Your opening role text for it: what it is responsible for and what it may do. It is quoted to the new assistant as your words, and it proposes its role line from it.",
		},
	}
	description := "Create a new assistant (a standing agent with its own home) that reports to you. Only an assistant's current thread may call it, never a worker. The assistant is created on this computer in your project and workspace with your provider and model, starts with no grants, and appears as its own sidebar row. Its first message tells it who created it and quotes role; it replies to you with thread_reply, and its questions and approvals come to you. Returns its assistant_id, name, home and the request token its replies answer. The answer arrives in this thread as a message."
	return tool("thread_new_assistant", description, properties, "name", "role")
}

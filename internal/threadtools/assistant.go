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
	Name     string   `json:"name"`
	Role     string   `json:"role"`
	Projects []string `json:"projects"`
}

// NewAssistantCall creates an assistant on the caller's computer.
type NewAssistantCall struct {
	Name string `json:"name"`
	// Role is the creator's opening role text, quoted to the new assistant
	// as the creator's words.
	Role string `json:"role"`
	// Projects are the ids of the projects the new assistant serves, each
	// one its creator serves; empty means every project the creator
	// serves.
	Projects []string `json:"projects,omitempty"`
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
	for _, project := range args.Projects {
		if project = trim(project); project != "" {
			call.Projects = append(call.Projects, project)
		}
	}
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
		"name":     map[string]any{"type": "string"},
		"role":     map[string]any{"type": "string"},
		"projects": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
	}
	description := "Create a sub-assistant reporting to you (assistants only; returns assistant_id, name, home, token)."
	return tool("thread_new_assistant", description, properties, "name", "role")
}

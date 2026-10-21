package app

import (
	"context"
	"errors"
	"fmt"
	"log"

	"agent-overflow/internal/agenthome"
	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
)

// NewAssistant is thread_new_assistant: an assistant creates another one
// (docs/specs/agent-thread-tools.md#thread_new_assistant). The new
// assistant is created as the user's New Assistant creates one, outside
// every project, serving projects its creator serves, with the settings a
// spawn inherits, and is started as the caller's spawn whose prompt is its
// opening block, so it answers its creator and its questions and approvals
// route there.
func (t threadToolsApp) NewAssistant(ctx context.Context, caller threadtools.Caller, call threadtools.NewAssistantCall) (threadtools.NewAssistantAck, error) {
	if threadtools.Forwarded(ctx) {
		return threadtools.NewAssistantAck{}, errorsx.Public(threadtools.CodeInvalidRequest,
			"thread_new_assistant creates an assistant on the caller's own computer and is never forwarded.", nil)
	}
	creator, found, err := t.app.store.GetStandingAgent(caller.ThreadID)
	if err != nil {
		return threadtools.NewAssistantAck{}, err
	}
	if !found {
		return threadtools.NewAssistantAck{}, errorsx.Public(threadtools.CodeInvalidRequest,
			"Only an assistant's current thread may create an assistant, and this thread is not one. Ask the user to create one with New Assistant.", nil)
	}
	origin, err := t.localOrigin(caller)
	if err != nil {
		return threadtools.NewAssistantAck{}, err
	}
	creatorThread, err := t.localThread(caller.ThreadID)
	if err != nil {
		return threadtools.NewAssistantAck{}, err
	}
	settings, err := t.spawnSettings(origin, threadtools.SpawnCall{})
	if err != nil {
		return threadtools.NewAssistantAck{}, err
	}
	if err := t.app.requireAutonomy(ctx, settings.RuntimeMode); err != nil {
		return threadtools.NewAssistantAck{}, err
	}
	projects, err := t.createdAssistantProjects(creator, call.Projects)
	if err != nil {
		return threadtools.NewAssistantAck{}, err
	}
	thread, agent, err := t.app.createAssistant(ctx, assistantSpec{
		name: call.Name, provider: settings.Provider, model: settings.Model, effort: settings.ReasoningEffort,
		runtimeMode: settings.RuntimeMode, projects: projects, creatorAgentID: creator.ID,
	})
	if err != nil {
		return threadtools.NewAssistantAck{}, publicAssistantNameError(err)
	}
	prompt := agenthome.CreatedOpeningMessage(thread.Title, agent.Home, creatorThread.Title, caller.ThreadID, call.Role)
	// The thread exists, so the request is written accepted and naming it.
	token := newThreadRequestToken()
	_, _, err = t.acceptRequest(origin, store.ThreadRequest{
		Token:          token,
		Kind:           store.ThreadRequestSpawn,
		TargetThreadID: thread.ID,
		Notify:         true,
	}, nil, false)
	if err == nil {
		err = t.dispatchRequest(ctx, origin, token, store.ThreadRequestSpawn, thread.ID, prompt, true)
	}
	var ack threadtools.RequestAck
	if err != nil {
		err = t.failRequest(token, err)
	} else {
		ack, err = t.ackRequest(ctx, caller, token, 0)
	}
	if err != nil {
		// The assistant exists with no opening message; the user sees its
		// row and the error, and can delete it or start the discussion.
		log.Printf("thread tools: assistant %s was created, but its opening request failed: %v", agent.ID, err)
		why := "it could not be sent"
		if _, public, ok := errorsx.PublicDetails(err); ok {
			why = public
		}
		return threadtools.NewAssistantAck{}, errorsx.Public(threadtools.CodeInvalidRequest,
			fmt.Sprintf("The assistant %q (thread %s) was created, but its opening message failed: %s Send it its role with thread_send, or ask the user to delete it.", thread.Title, thread.ID, why), err)
	}
	return threadtools.NewAssistantAck{RequestAck: ack, AssistantID: agent.ID, Name: thread.Title, Home: agent.Home}, nil
}

// publicAssistantNameError is a name refusal the model can read.
func publicAssistantNameError(err error) error {
	var taken *store.AssistantNameTakenError
	switch {
	case errors.As(err, &taken):
		return errorsx.Public(threadtools.CodeInvalidRequest,
			fmt.Sprintf("The assistant %q (thread %s) already has that name; names are unique on this computer, ignoring case. Choose another name.", taken.Name, taken.ThreadID), err)
	case errors.Is(err, store.ErrAssistantNameEmpty):
		return errorsx.Public(threadtools.CodeInvalidRequest, "name is required and cannot be blank.", err)
	}
	return err
}

// createdAssistantProjects are the projects an assistant creator's new
// assistant serves: the ones named, each of which the creator must serve
// (requireServedProject), or every one the creator serves when none is
// named.
func (t threadToolsApp) createdAssistantProjects(creator store.StandingAgent, named []string) (assistantProjects, error) {
	if len(named) == 0 {
		if creator.AllProjects {
			return assistantProjects{all: true}, nil
		}
		ids, err := t.app.store.AgentProjectIDs(creator.ID)
		if err != nil {
			return assistantProjects{}, err
		}
		if len(ids) == 0 {
			return assistantProjects{}, errorsx.Public(threadtools.CodeInvalidRequest,
				"You serve no project, so the new assistant would serve none. Ask the user to give you a project from your menu.", nil)
		}
		return assistantProjects{ids: ids}, nil
	}
	for _, id := range named {
		if err := t.requireServedProject(creator, id); err != nil {
			return assistantProjects{}, err
		}
	}
	return assistantProjects{ids: named}, nil
}

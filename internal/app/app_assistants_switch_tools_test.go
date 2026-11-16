package app

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"agent-overflow/internal/threadtools"
)

// While assistants are off, agents are not offered them: a session's tools
// and guide leave them out (the guide Claude reads from the handshake and
// Codex from its developer instructions are the same text), an assistant's
// name or thread is no address and saying so, the listing and the search
// leave assistants' threads out, and creating or replacing one is refused,
// from a session that started before the switch too. On again, all of it
// is back.
func TestAgentsAreNotOfferedAssistantsWhileOff(t *testing.T) {
	t.Parallel()
	f := newAssistantFixture(t)
	a := f.app
	a.registerSettingsBroadcast()
	target, _ := f.assistantTarget(t, "Ops.")
	if err := a.store.UpdateTitle(target.ID, "Ops"); err != nil {
		t.Fatal(err)
	}
	listed := func() []string {
		t.Helper()
		page, err := f.adapter().SearchThreads(t.Context(), threadtools.SearchQuery{Limit: 50})
		if err != nil {
			t.Fatalf("list threads: %v", err)
		}
		var ids []string
		for _, row := range page.Rows {
			ids = append(ids, row.Thread.ID)
		}
		return ids
	}
	toolNames := func() []string {
		var names []string
		for _, tool := range a.threadToolsServer().Tools(a.threadToolsShape(f.caller.ID, subThreadPlace{})) {
			names = append(names, tool["name"].(string))
		}
		return names
	}
	if !slices.Contains(listed(), target.ID) || !slices.Contains(toolNames(), "thread_new_assistant") {
		t.Fatal("the fixture's assistant is not offered while assistants are on")
	}

	setAssistants(t, a, false)

	if names := toolNames(); slices.Contains(names, "thread_new_assistant") || slices.Contains(names, "thread_watch") {
		t.Errorf("the tools with assistants off = %v", names)
	}
	guide := a.threadToolsServer().Instructions(a.threadToolsShape(f.caller.ID, subThreadPlace{}))
	for _, word := range []string{"assistant'", "An assistant", "standing agent", "thread_watch"} {
		if strings.Contains(guide, word) {
			t.Errorf("the guide with assistants off says %q", word)
		}
	}
	for _, ref := range []string{"Ops", target.ID} {
		if _, err := f.adapter().ResolveThreadRef(t.Context(), ref); !errors.Is(err, errAssistantsOff) {
			t.Errorf("resolve %q = %v, want it refused as assistants turned off", ref, err)
		}
		// A paired computer's caller is told which computer has them off.
		forwarded := threadtools.WithForwarded(t.Context())
		want := "Assistants are turned off in Settings on " + a.backendDisplayName() + "."
		if _, err := f.adapter().ResolveThreadRef(forwarded, ref); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("forwarded resolve %q = %v, want %q", ref, err, want)
		}
	}
	if slices.Contains(listed(), target.ID) {
		t.Error("the listing shows an assistant's thread while assistants are off")
	}
	if _, err := f.adapter().SearchThreads(t.Context(), threadtools.SearchQuery{Agents: true}); !errors.Is(err, errAssistantsOff) {
		t.Errorf("a search for assistants = %v, want it refused", err)
	}
	if _, err := a.CreateAssistant(t.Context(), CreateAssistantOptions{Name: "Second", AllProjects: true}); !errors.Is(err, errAssistantsOff) {
		t.Errorf("New Assistant = %v, want it refused", err)
	}
	// A session started before the switch still lists the tool; its call is
	// refused with the same words.
	if _, err := f.adapter().NewAssistant(t.Context(), f.targetIdentity(t, target.ID), threadtools.NewAssistantCall{Name: "Third", Role: "r"}); err == nil || !strings.Contains(err.Error(), assistantsOffMessage) {
		t.Errorf("thread_new_assistant while off = %v, want %q", err, assistantsOffMessage)
	}
	if _, err := a.StartNextAssistantThread(t.Context(), target.ID); !errors.Is(err, errAssistantsOff) {
		t.Errorf("New Thread, Same Assistant = %v, want it refused", err)
	}

	setAssistants(t, a, true)
	if res, err := f.adapter().ResolveThreadRef(t.Context(), "Ops"); err != nil || len(res.Matches) != 1 || res.Matches[0].ThreadID != target.ID {
		t.Fatalf("resolve Ops after turning assistants on = %+v, %v", res, err)
	}
	if !slices.Contains(listed(), target.ID) || !slices.Contains(toolNames(), "thread_new_assistant") {
		t.Fatal("assistants are not offered again once turned on")
	}
}

// While assistants are off, a moment of an assistant's thread is a hidden
// thread's, which raises nothing unless the screen opted into hidden
// threads; an ordinary thread's is not. On again, it raises as before.
func TestAPausedAssistantsThreadIsHiddenFromNotifications(t *testing.T) {
	t.Parallel()
	f := newAssistantFixture(t)
	a := f.app
	a.registerSettingsBroadcast()
	target, _ := f.assistantTarget(t, "Ops.")

	setAssistants(t, a, false)
	if !a.notificationThreadRef(target.ID).Hidden {
		t.Error("an assistant's thread is not hidden from notifications while assistants are off")
	}
	if a.notificationThreadRef(f.caller.ID).Hidden {
		t.Error("an ordinary thread is hidden from notifications while assistants are off")
	}
	setAssistants(t, a, true)
	if a.notificationThreadRef(target.ID).Hidden {
		t.Error("an assistant's thread stays hidden from notifications once assistants are on")
	}
}

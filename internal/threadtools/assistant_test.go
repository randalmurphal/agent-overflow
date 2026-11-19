package threadtools

import (
	"context"
	"strings"
	"testing"
)

const (
	localAgentThreadID  = "aaaaaaaa-1111-4111-8111-111111111111"
	remoteAgentThreadID = "bbbbbbbb-2222-4222-8222-222222222222"
)

func assistantThread(id, name string) Thread {
	return Thread{ID: id, Title: name, Assistant: &Assistant{ID: "agent-" + id[:4], Name: name}}
}

// TestAssistantNameResolvesBelowThePrefixLength: a short name addresses the
// assistant's current thread, ignoring case, while a short id prefix still
// names nothing.
func TestAssistantNameResolvesBelowThePrefixLength(t *testing.T) {
	app := newFakeApp("Laptop")
	app.addThread(assistantThread(localAgentThreadID, "Ops"))
	app.addThread(Thread{ID: localThreadID, Title: "Other"})

	target, err := soloSession(app).resolve(context.Background(), "ops", "")
	if err != nil || target.ThreadID != localAgentThreadID {
		t.Fatalf("resolve ops = %+v, %v; want the assistant's thread", target, err)
	}
	if _, err := soloSession(app).resolve(context.Background(), "1111", ""); err == nil {
		t.Fatal("a four-character id prefix resolved")
	}
}

// TestShortReferenceIgnoresAPeerThatPredatesNames: a computer that answers
// a short reference from its ids is not taken as having matched a name.
func TestShortReferenceIgnoresAPeerThatPredatesNames(t *testing.T) {
	p := newPair(t)
	p.remote.prefixOnly = true
	p.remote.addThread(Thread{ID: remoteThreadID, Title: "Remote"})

	_, err := p.session().resolve(context.Background(), "2222", "")
	if code, _ := publicMessage(err); code != CodeNotFound {
		t.Fatalf("a short prefix matched an old peer's id: %v", err)
	}
}

// TestSameNameOnTwoComputersIsAmbiguousUntilSuffixed: the refusal lists
// each assistant as <name>-<computer>, and that form resolves to the one
// on the named computer.
func TestSameNameOnTwoComputersIsAmbiguousUntilSuffixed(t *testing.T) {
	p := newPair(t)
	p.local.addThread(assistantThread(localAgentThreadID, "Ops"))
	p.remote.addThread(assistantThread(remoteAgentThreadID, "ops"))

	_, err := p.session().resolve(context.Background(), "Ops", "")
	code, message := publicMessage(err)
	if code != CodeAmbiguous || !strings.Contains(message, `"Ops-Laptop"`) || !strings.Contains(message, `"ops-Studio"`) {
		t.Fatalf("ambiguous name refusal = %s %q", code, message)
	}
	target, err := p.session().resolve(context.Background(), "Ops-studio", "")
	if err != nil || target.ThreadID != remoteAgentThreadID || target.Local {
		t.Fatalf("resolve Ops-studio = %+v, %v; want the Studio assistant", target, err)
	}
	target, err = p.session().resolve(context.Background(), "ops-Laptop", "")
	if err != nil || target.ThreadID != localAgentThreadID || !target.Local {
		t.Fatalf("resolve ops-Laptop = %+v, %v; want the local assistant", target, err)
	}
}

// TestSearchSuffixesAClashingAssistantName and only that one, and passes
// the agents filter on to the peer.
func TestSearchSuffixesAClashingAssistantName(t *testing.T) {
	p := newPair(t)
	p.local.hits = []Hit{
		{Thread: assistantThread(localAgentThreadID, "Ops")},
		{Thread: assistantThread(localThreadID, "Release")},
	}
	p.remote.hits = []Hit{{Thread: assistantThread(remoteAgentThreadID, "OPS")}}

	result := call(t, p.server, localCaller(), "thread_search", `{"agents":true}`)
	groups := rows(t, result["computers"])
	names := map[string]string{}
	for _, group := range groups {
		for _, row := range rows(t, field(t, group, "rows")) {
			fields := row.(map[string]any)
			names[fields["thread_id"].(string)] = fields["assistant"].(map[string]any)["name"].(string)
		}
	}
	want := map[string]string{localAgentThreadID: "Ops-Laptop", localThreadID: "Release", remoteAgentThreadID: "OPS-Studio"}
	for id, name := range want {
		if names[id] != name {
			t.Errorf("assistant name of %s = %q, want %q", id, names[id], name)
		}
	}
	if !p.local.searches[0].Agents || !p.remote.searches[0].Agents {
		t.Errorf("agents filter not applied on both computers: local %+v remote %+v", p.local.searches[0], p.remote.searches[0])
	}
}

// TestNewAssistantValidatesBeforeTheApp: a blank or multi-line name and a
// blank role cost one call and reach nothing.
func TestNewAssistantValidatesBeforeTheApp(t *testing.T) {
	app := newFakeApp("Laptop")
	server := New(app)
	for _, args := range []string{`{"name":" ","role":"r"}`, `{"name":"a\nb","role":"r"}`, `{"name":"Ops","role":" "}`} {
		if _, err := server.Call(context.Background(), localCaller(), "thread_new_assistant", []byte(args)); err == nil {
			t.Errorf("%s was accepted", args)
		}
	}
	if len(app.assistants) != 0 {
		t.Fatalf("the app was asked: %+v", app.assistants)
	}
	result := call(t, server, localCaller(), "thread_new_assistant", `{"name":" Ops ","role":"Keeps the build green."}`)
	if result["assistant_id"] != "agent-1" || result["name"] != "Ops" || result["kind"] != "spawn" {
		t.Fatalf("result = %v", result)
	}
	if got := app.assistants[0]; got.Name != "Ops" || got.Role != "Keeps the build green." {
		t.Fatalf("call = %+v", got)
	}
}

// TestNewAssistantIsNeverForwarded: a peer cannot create an assistant here.
func TestNewAssistantIsNeverForwarded(t *testing.T) {
	app := newFakeApp("Laptop")
	if _, err := New(app).CallForwarded(context.Background(), localCaller(), "thread_new_assistant", []byte(`{"name":"Ops","role":"r"}`)); err == nil {
		t.Fatal("a forwarded thread_new_assistant was accepted")
	}
	if len(app.assistants) != 0 {
		t.Fatalf("the app was asked: %+v", app.assistants)
	}
}

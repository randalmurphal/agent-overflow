package threadtools

import (
	"context"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// assistantPattern matches the words the guide of a session with assistants
// turned off never has. "assistant message" and "assistant text" are the provider's
// role, not this feature, and are allowed.
var assistantPattern = regexp.MustCompile(`(?i)standing agent|assistant(?:s|'s)?\b(?: (?:message|text|messages))?|thread_watch|home_files|AO_AGENT_HOME|\bwatch(?:es)?\b|agent home`)

func assistantMentions(text string) []string {
	var found []string
	for _, match := range assistantPattern.FindAllString(text, -1) {
		lower := strings.ToLower(match)
		if strings.HasSuffix(lower, " message") || strings.HasSuffix(lower, " messages") || strings.HasSuffix(lower, " text") {
			continue
		}
		found = append(found, match)
	}
	return found
}

// With assistants turned off, a session's tools and guide say nothing of
// assistants: no thread_new_assistant or thread_watch, no home_files or
// agents parameter, no sentence about them, on one computer and paired,
// with spawning or at the sub-thread limit. The guide still names only
// what the tools have.
func TestAssistantsOffShapeSaysNothingOfAssistants(t *testing.T) {
	atLimit := soloShape()
	atLimit.Spawn = SpawnShape{Level: 1, Limit: 1}
	for _, base := range []Shape{soloShape(), pairedShape(), atLimit} {
		shape := base
		shape.AssistantsOff = true
		if found := assistantMentions(instructionsFor(shape)); len(found) > 0 {
			t.Errorf("paired=%v limit=%v: the guide mentions %q", shape.Paired(), shape.Spawn.Limit, found)
		}
		// The tools a session can call, and the parameters it can pass:
		// shared tools keep their descriptions, which mention loops and
		// watches together.
		tools := New(newFakeApp("laptop")).Tools(shape)
		for _, tool := range tools {
			for property := range schemaProperties(t, tool) {
				if property == "home_files" || property == "agents" {
					t.Errorf("paired=%v: tool %s takes %s", shape.Paired(), tool["name"], property)
				}
			}
		}
		names := toolsByName(t, shape)
		for _, gone := range []string{"thread_new_assistant", "thread_watch"} {
			if _, ok := names[gone]; ok {
				t.Errorf("paired=%v: the tools list %s", shape.Paired(), gone)
			}
		}
		// The same shape with assistants on keeps all of it.
		base.AssistantsOff = false
		on := toolsByName(t, base)
		for _, kept := range []string{"thread_new_assistant", "thread_watch"} {
			if _, ok := on[kept]; !ok {
				t.Errorf("paired=%v: assistants on lost %s", base.Paired(), kept)
			}
		}
		if len(assistantMentions(instructionsFor(base))) == 0 {
			t.Errorf("paired=%v: the guide with assistants on says nothing of them", base.Paired())
		}
	}
	// Every sentence the off shape drops is in the paragraph it is dropped
	// from, so none survives a rewrite of its paragraph unnoticed.
	whole := instructionsAnswering + instructionsOrganizing + instructionsIds
	for _, sentence := range assistantSentences {
		if !strings.Contains(whole, sentence) {
			t.Errorf("the guide no longer has %q", sentence)
		}
	}
	if !strings.Contains(instructionsStopping, "With a `label` it stops your loop or watch.") {
		t.Error("the Stopping paragraph no longer has the sentence the off shape rewrites")
	}
}

// Each computer answers for its own assistants by its own setting: a caller
// reaches a paired computer's assistant by name and finds it in search
// whatever the caller's computer has, and a computer whose own assistants
// are off refuses the address, which the caller reads as that refusal
// rather than as a computer that did not answer.
func TestAPairedComputersAssistantsFollowItsOwnSetting(t *testing.T) {
	p := newPair(t)
	p.remote.addThread(assistantThread(remoteAgentThreadID, "Ops"))
	p.remote.addThread(Thread{ID: remoteThreadID, Title: "Remote"})
	p.remote.hits = []Hit{{Thread: assistantThread(remoteAgentThreadID, "Ops")}, {Thread: Thread{ID: remoteThreadID, Title: "Remote"}}}

	if target, err := p.session().resolve(context.Background(), "Ops", ""); err != nil || target.ThreadID != remoteAgentThreadID {
		t.Fatalf("resolve a paired assistant = %+v, %v", target, err)
	}
	var ids []string
	for _, group := range rows(t, call(t, p.server, localCaller(), "thread_search", `{}`)["computers"]) {
		for _, row := range rows(t, field(t, group, "rows")) {
			ids = append(ids, row.(map[string]any)["thread_id"].(string))
		}
	}
	if !slices.Contains(ids, remoteAgentThreadID) || !slices.Contains(ids, remoteThreadID) {
		t.Errorf("search listed %v; want the paired assistant and the plain thread", ids)
	}

	p.remote.failWith = publicf(CodeUnsupported, "Assistants are turned off in Settings on Studio.")
	_, err := p.session().resolve(context.Background(), "Ops", "")
	if code, message := publicMessage(err); code != CodeUnsupported || message != "Assistants are turned off in Settings on Studio." {
		t.Fatalf("resolve an assistant its computer has off = %s %q", code, message)
	}
}

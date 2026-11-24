package codex

import (
	"encoding/json"
	"fmt"
	"testing"

	"agent-overflow/internal/provider"
)

// collab_agents_generations_test.go — the per-child turn-generation counter
// separates two identical deliveries from one child (subagentNotificationDedupKey).
// It is keyed by the child's canonical name and outlives close_agent with the
// rest of the child's identity.

// v1RootSession is recordingRootSession with the metadata read a V1 spawn
// schedules answered: every child is called Ada.
func v1RootSession(t *testing.T) (*connFixture, *Session, func() []provider.ProviderEvent) {
	t.Helper()
	fx, s, events := recordingRootSession(t)
	fx.answer("thread/read", func(params json.RawMessage) any {
		return map[string]any{"thread": map[string]any{"id": readNestedString(params, "threadId"), "agentNickname": "Ada"}}
	})
	return fx, s, events
}

func v1CollabLine(itemID, tool, status, receiverThreadID string) []byte {
	return []byte(fmt.Sprintf(`{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"root-provider-thread","turnId":"root-turn","item":{"id":%q,"type":"collabAgentToolCall","tool":%q,"receiverThreadIds":[%q],"status":%q}}}`, itemID, tool, receiverThreadID, status))
}

// v1Spawn feeds both legs of a V1 spawn_agent item, as Codex sends them.
func v1Spawn(fx *connFixture, itemID, receiverThreadID string) {
	fx.feed([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","method":"item/started","params":{"threadId":"root-provider-thread","turnId":"root-turn","item":{"id":%q,"type":"collabAgentToolCall","tool":"spawnAgent","receiverThreadIds":[],"status":"inProgress"}}}`, itemID)))
	fx.feed(v1CollabLine(itemID, "spawnAgent", "completed", receiverThreadID))
}

func childTurnStarted(threadID, turnID string) []byte {
	return []byte(fmt.Sprintf(`{"jsonrpc":"2.0","method":"turn/started","params":{"threadId":%q,"turn":{"id":%q}}}`, threadID, turnID))
}

func v1CompletionCarrier(itemID, childThreadID, answer string) []byte {
	return []byte(fmt.Sprintf(`{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"root-provider-thread","turnId":"root-turn","item":{"id":%q,"type":"userMessage","content":[{"type":"text","text":"<subagent_notification>{\"agent_path\":\"%s\",\"status\":{\"completed\":\"%s\"}}</subagent_notification>"}]}}}`, itemID, childThreadID, answer))
}

// A V1 child that answers the same thing before a close and again after a
// resume gets both deliveries. The turn after the resume counts on from the
// child's turns before the close; a count that restarted at the close would
// give the second answer the first one's dedup key and drop it.
func TestAResumedChildsRepeatedAnswerIsDeliveredAgain(t *testing.T) {
	t.Parallel()
	fx, s, events := v1RootSession(t)
	v1Spawn(fx, "spawn-a", "child-a")
	fx.feed(childTurnStarted("child-a", "child-turn-1"))
	fx.feed(v1CompletionCarrier("carrier-1", "child-a", "Done."))
	fx.feed(v1CollabLine("close-a", "closeAgent", "completed", "child-a"))
	fx.feed(v1CollabLine("resume-a", "resumeAgent", "completed", "child-a"))
	fx.feed(childTurnStarted("child-a", "child-turn-2"))
	fx.feed(v1CompletionCarrier("carrier-2", "child-a", "Done."))

	s.collabAsyncWG.Wait()
	deliveries := 0
	for _, event := range events() {
		if event.Kind == provider.EventSubagentNotification && event.ItemID == "spawn-a" {
			deliveries++
		}
	}
	if deliveries != 2 {
		t.Fatalf("spawn-a received %d completion deliveries, want one before the close and one after the resume: %+v", deliveries, events())
	}
}

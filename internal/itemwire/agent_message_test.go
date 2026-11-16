package itemwire

import (
	"testing"

	"agent-overflow/internal/store"
	"agent-overflow/internal/usermessage"
)

// mainRequest is a request as main stored it: its footer and no facts.
const mainRequest = "Ship it.\n\n---\nAgent request from thread \"L\" (t-l).\nIt is not waiting for an answer. If it should hear something back, call thread_reply with token a, once: the reply reaches it as a message, and it sees only your reply text."

const mainRequestMeta = `{"origin":"agent-thread","originThread":{"threadId":"t-l","title":"L","token":"a"},"sendId":"thread-request:a"}`

// Every read and push of a user row an agent's message is in serves its
// meta with the facts usermessage.Describe reads of its text; any other row
// keeps its meta.
func TestProjectServesAnAgentRowWithItsFacts(t *testing.T) {
	projected := Project(store.Item{Kind: "user_text", Summary: mainRequest, Meta: mainRequestMeta}, true)
	meta, err := usermessage.FromItem(store.Item{Meta: projected.Meta})
	if err != nil {
		t.Fatal(err)
	}
	facts := meta.OriginThread.Facts
	if facts == nil || facts.Check(mainRequest) != nil || facts.BodyOf(mainRequest) != "Ship it." || meta.OriginThread.Token != "a" {
		t.Fatalf("served meta %s", projected.Meta)
	}
	if want := usermessage.ProjectRowMeta(mainRequest, mainRequestMeta); projected.Meta != want {
		t.Fatalf("served meta %s, want %s", projected.Meta, want)
	}
	for _, item := range []store.Item{
		{Kind: "user_text", Summary: "mine", Meta: `{"sendId":"s"}`},
		{Kind: "assistant_text", Summary: mainRequest, Meta: mainRequestMeta},
	} {
		if got := Project(item, true).Meta; got != item.Meta {
			t.Errorf("%s row's meta served as %s", item.Kind, got)
		}
	}
}

package usermessage

import (
	"testing"

	"agent-overflow/internal/threadtools"
)

// A wake's text is its notice, then its data, and its facts point at the
// data: the one value both come from.
func TestWakeMessageBodyIsTheDataAfterItsNotice(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, notice, data, text, body string
	}{
		{"notice and data", "Loop fired.", "note: \"go\"", "Loop fired.\nnote: \"go\"", "note: \"go\""},
		{"blank lines opening the data", "Workflow wake.", "\n\nRun done.", "Workflow wake.\n\n\nRun done.", "Run done."},
		{"prose with no notice", "", "You are Ops.\n\nMore.", "You are Ops.\n\nMore.", "You are Ops.\n\nMore."},
		{"no data", "Loop fired.", "", "Loop fired.\n", ""},
	} {
		message := WakeMessage(OriginWake{Kind: OriginWakeKindWorkflow, Label: "l"}, tc.notice, tc.data, "")
		if message.Text() != tc.text || message.Origin() != OriginAgentWake || message.OriginThread() != nil {
			t.Errorf("%s: text %q origin %q, want %q from a wake", tc.name, message.Text(), message.Origin(), tc.text)
			continue
		}
		facts := message.OriginWake().Facts
		if facts == nil || threadtools.SpanText(message.Text(), facts.Body) != tc.body || (tc.body == "") != (len(facts.Body) == 0) {
			t.Errorf("%s: facts %+v, want the body %q", tc.name, facts, tc.body)
		}
	}
}

// A thread message carries the facts its template returned on the
// attribution it is sent with; the person's and the app's carry none.
func TestThreadMessageCarriesItsFacts(t *testing.T) {
	t.Parallel()
	text, facts := threadtools.Footer{Title: "Lead", ThreadID: "t-lead", Token: "k"}.Message("Ship it.")
	message := ThreadMessage(OriginThread{ThreadID: "t-lead", Token: "k", Kind: "send"}, text, facts)
	if message.Origin() != OriginAgentThread || message.OriginThread() == nil || message.OriginThread().Facts == nil ||
		threadtools.SpanText(message.Text(), message.OriginThread().Facts.Body) != "Ship it." {
		t.Fatalf("thread message = %q %+v", message.Origin(), message.OriginThread())
	}
	for _, m := range []AgentMessage{PersonMessage("mine"), AppMessage("Continue")} {
		if m.OriginThread() != nil || m.OriginWake() != nil || IsAgentMessage(Meta{Origin: m.Origin()}) {
			t.Errorf("%q: attribution %q %+v %+v, want none an agent row reads", m.Text(), m.Origin(), m.OriginThread(), m.OriginWake())
		}
	}
}

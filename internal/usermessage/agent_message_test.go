package usermessage

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
)

// A report's text is its notice, then its data, and its facts point at the
// data: the one value both come from.
func TestReportBodyIsTheDataAfterItsNotice(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, notice, data, text, body string
	}{
		{"notice and data", "Loop fired.", "note: \"go\"", "Loop fired.\nnote: \"go\"", "note: \"go\""},
		{"blank lines opening the data", "Workflow wake.", "\n\nRun done.", "Workflow wake.\n\n\nRun done.", "Run done."},
		{"prose with no notice", "", "You are Ops.\n\nMore.", "You are Ops.\n\nMore.", "You are Ops.\n\nMore."},
		{"no data", "Loop fired.", "", "Loop fired.\n", ""},
	} {
		message := Report(ReportWorkflow, Reporter{Label: "l"}, tc.notice, tc.data, "")
		attribution := message.Attribution()
		if message.Text() != tc.text || attribution.Origin != OriginAgentWake || attribution.Thread != nil || attribution.Wake.Kind != OriginWakeKindWorkflow {
			t.Errorf("%s: text %q attribution %+v, want %q from a workflow", tc.name, message.Text(), attribution, tc.text)
			continue
		}
		facts := attribution.Wake.Facts
		if facts == nil || facts.Form != threadtools.FormReport || facts.Check(message.Text()) != nil ||
			facts.BodyOf(message.Text()) != tc.body || (tc.body == "") != (len(facts.Body) == 0) {
			t.Errorf("%s: facts %+v, want the body %q", tc.name, facts, tc.body)
		}
	}
}

// Every ReportKind is an OriginWake kind a stored report may carry, and the
// reader names it (agentMessage.ts WAKE_ORIGIN_KINDS).
func TestReportKindsAreTheKindsAStoredReportCarries(t *testing.T) {
	t.Parallel()
	for _, kind := range reportKinds {
		message := Report(kind, Reporter{Label: "l"}, "", "data", "")
		attribution := message.Attribution()
		if _, err := DecodeStored(message.Text(), attribution.Origin, attribution.Thread, attribution.Wake); err != nil {
			t.Errorf("%s: %v", kind.name, err)
		}
	}
}

// A request is attributed to the thread its footer names, under the
// footer's token: a sender the footer does not name cannot be written, nor
// a kind that is no request's.
func TestRequestIsAttributedToTheThreadItsFooterNames(t *testing.T) {
	t.Parallel()
	footer := threadtools.Footer{Title: "Lead", ThreadID: "t-lead", Token: "k", AnswerRequested: true}
	from := Sender{ComputerID: "c-1", ComputerName: "Desk", ThreadID: "t-lead", Title: "Lead"}
	message, err := Request(store.ThreadRequestSpawn, from, footer, "Ship it.")
	if err != nil {
		t.Fatal(err)
	}
	thread := message.Attribution().Thread
	if thread == nil || thread.ThreadID != "t-lead" || thread.Title != "Lead" || thread.Token != "k" || thread.Kind != "spawn" ||
		thread.ComputerID != "c-1" || thread.Facts == nil || thread.Facts.Form != threadtools.FormRequest ||
		thread.Facts.Check(message.Text()) != nil || thread.Facts.BodyOf(message.Text()) != "Ship it." {
		t.Fatalf("request = %q %+v", message.Text(), thread)
	}
	for name, sender := range map[string]Sender{
		"another thread":   {ThreadID: "t-other", Title: "Lead"},
		"another title":    {ThreadID: "t-lead", Title: "Someone else"},
		"no thread at all": {},
	} {
		if _, err := Request(store.ThreadRequestSend, sender, footer, "x"); err == nil {
			t.Errorf("%s: written", name)
		}
	}
	if _, err := Request(store.ThreadRequestRemind, from, footer, "x"); err == nil {
		t.Error("a reminder written as a request")
	}
}

// An answer is attributed to the first thread it speaks for, under that
// thread's first token, from the wake it renders.
func TestAnswerIsAttributedToItsFirstSource(t *testing.T) {
	t.Parallel()
	message := Answer(threadtools.Wake{Kind: threadtools.WakeReply, Text: "Same.", Sources: []threadtools.WakeSource{
		{Title: "W", ThreadID: "t-w", Computer: "Desk", ComputerID: "c-1", Tokens: []string{"a", "b"}},
		{Title: "X", ThreadID: "t-x", Tokens: []string{"c"}},
	}})
	thread := message.Attribution().Thread
	if thread == nil || thread.ThreadID != "t-w" || thread.Title != "W" || thread.Token != "a" || thread.ComputerID != "c-1" ||
		thread.ComputerName != "Desk" || thread.Kind != OriginKindWake || thread.Facts == nil || thread.Facts.Check(message.Text()) != nil {
		t.Fatalf("answer = %+v", thread)
	}
	reminder := Answer(threadtools.Wake{Kind: threadtools.WakeReminder, Text: "Check.", Sources: []threadtools.WakeSource{{Tokens: []string{"r"}}}})
	if thread := reminder.Attribution().Thread; thread.ThreadID != "" || thread.Token != "r" || thread.Facts.Outcome != threadtools.WakeReminder {
		t.Fatalf("reminder = %+v", thread)
	}
}

// A plain message is its sender's words, all of them its body, with no
// request or answer about it.
func TestMessageIsItsSendersWords(t *testing.T) {
	t.Parallel()
	message := Message(Sender{ThreadID: "t-lead", Title: "Lead"}, "Question: Q?\nAnswer: A")
	thread := message.Attribution().Thread
	if message.Text() != "Question: Q?\nAnswer: A" || thread == nil || thread.ThreadID != "t-lead" || thread.Token != "" || thread.Kind != "" ||
		thread.Facts.Form != threadtools.FormMessage || thread.Facts.BodyOf(message.Text()) != message.Text() {
		t.Fatalf("message = %q %+v", message.Text(), thread)
	}
}

// Attribution is a copy: nothing done to it changes the message.
func TestAttributionIsACopy(t *testing.T) {
	t.Parallel()
	message := Answer(threadtools.Wake{Kind: threadtools.WakeReply, Text: "Done.", Sources: []threadtools.WakeSource{{Title: "W", ThreadID: "t-w", Tokens: []string{"a"}}}})
	before := message.Attribution()
	changed := message.Attribution()
	changed.Thread.ThreadID, changed.Thread.Title = "t-other", "Other"
	changed.Thread.Facts.Body[0].Start, changed.Thread.Facts.Tokens[0] = 0, "x"
	if after := message.Attribution(); !reflect.DeepEqual(after, before) {
		t.Fatalf("the message changed: %+v, was %+v", after.Thread, before.Thread)
	}
	report := Report(ReportLoop, Reporter{Label: "l"}, "n", "d", "")
	report.Attribution().Wake.Facts.Body[0].End = 1
	if facts := report.Attribution().Wake.Facts; facts.Check(report.Text()) != nil {
		t.Fatalf("the report changed: %+v", facts)
	}
}

// A built message shares nothing with what it was built from: a caller that
// changes its footer, wake or notice afterward changes neither the text nor
// any field of the attribution.
func TestAMessageSharesNothingWithItsInputs(t *testing.T) {
	t.Parallel()
	footer := threadtools.Footer{Title: "Lead", ThreadID: "t-lead", Token: "k", Agent: &threadtools.SenderAgent{Role: "Plans."}}
	single := threadtools.Wake{Kind: threadtools.WakeReply, Text: "Done.", Sources: []threadtools.WakeSource{
		{Title: "W", ThreadID: "t-w", Tokens: []string{"a", "b"}, Agent: &threadtools.SenderAgent{Role: "Builds."}},
	}}
	digest := threadtools.Wake{Kind: threadtools.WakeReply, Text: "Same.", Sources: []threadtools.WakeSource{
		{Title: "W", ThreadID: "t-w", Tokens: []string{"a", "b"}, Agent: &threadtools.SenderAgent{Role: "Builds."}},
		{Title: "X", ThreadID: "t-x", ComputerID: "c-1", Computer: "Desk", Tokens: []string{"c"}, Agent: &threadtools.SenderAgent{Role: "Lexes."}},
	}}
	stopped := threadtools.WorkerStopped{Title: "W", ThreadID: "t-w", Token: "k", Failures: []string{"its turn"}}
	build := map[string]func(t *testing.T) AgentMessage{
		"request": func(t *testing.T) AgentMessage {
			message, err := Request(store.ThreadRequestSend, Sender{ThreadID: "t-lead", Title: "Lead"}, footer, "Ship it.")
			if err != nil {
				t.Fatal(err)
			}
			return message
		},
		"answer":         func(*testing.T) AgentMessage { return Answer(single) },
		"digest":         func(*testing.T) AgentMessage { return Answer(digest) },
		"stopped worker": func(*testing.T) AgentMessage { return StoppedWorker(stopped, "c-1", "Desk") },
		"message":        func(*testing.T) AgentMessage { return Message(Sender{ThreadID: "t-lead", Title: "Lead"}, "Hi.") },
		"report":         func(*testing.T) AgentMessage { return Report(ReportLoop, Reporter{Label: "l"}, "n", "d", "h") },
	}
	built := map[string]AgentMessage{}
	before := map[string]Attribution{}
	for name, construct := range build {
		built[name] = construct(t)
		before[name] = built[name].Attribution()
	}
	texts := map[string]string{}
	for name, message := range built {
		texts[name] = message.Text()
	}
	footer.Agent.Role = "changed"
	for _, wake := range []threadtools.Wake{single, digest} {
		for i := range wake.Sources {
			wake.Sources[i].Tokens[0] = "changed"
			wake.Sources[i].Agent.Role = "changed"
			wake.Sources[i].Title = "changed"
		}
	}
	stopped.Failures[0] = "changed"
	for name, message := range built {
		if message.Text() != texts[name] {
			t.Errorf("%s: text changed to %q", name, message.Text())
		}
		if after := message.Attribution(); !reflect.DeepEqual(after, before[name]) {
			t.Errorf("%s: attribution changed:\n%s\nwas\n%s", name, attributionJSON(t, after), attributionJSON(t, before[name]))
		}
	}
}

func attributionJSON(t *testing.T, attribution Attribution) string {
	t.Helper()
	encoded, err := json.Marshal(attribution)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

// The person's and the app's messages carry no attribution an agent row
// reads.
func TestPersonAndAppMessagesAreNoAgents(t *testing.T) {
	t.Parallel()
	for _, m := range []AgentMessage{PersonMessage("mine"), AppMessage("Continue")} {
		attribution := m.Attribution()
		if attribution.Thread != nil || attribution.Wake != nil || IsAgentMessage(Meta{Origin: attribution.Origin}) {
			t.Errorf("%q: attribution %+v, want none an agent row reads", m.Text(), attribution)
		}
	}
}

// DecodeStored reads back what the constructors wrote and refuses what none
// writes, keeping the text and its sender with facts that claim nothing.
func TestDecodeStoredRefusesWhatNoConstructorWrites(t *testing.T) {
	t.Parallel()
	request, err := Request(store.ThreadRequestSend, Sender{ThreadID: "t-lead", Title: "Lead"},
		threadtools.Footer{Title: "Lead", ThreadID: "t-lead", Token: "k"}, "Ship it.")
	if err != nil {
		t.Fatal(err)
	}
	written := request.Attribution()
	read, err := DecodeStored(request.Text(), written.Origin, written.Thread, written.Wake)
	if err != nil || !reflect.DeepEqual(read, request) {
		t.Fatalf("read back %+v, %v", read, err)
	}
	// main's request has no facts: kept for the legacy reader.
	if _, err := DecodeStored("old", OriginAgentThread, &OriginThread{ThreadID: "t"}, nil); err != nil {
		t.Fatalf("main's request: %v", err)
	}
	wake := Report(ReportLoop, Reporter{Label: "l"}, "n", "d", "").Attribution().Wake
	for name, tc := range map[string]struct {
		text, origin string
		thread       *OriginThread
		wake         *OriginWake
	}{
		"text edited behind its facts":  {request.Text() + " more", OriginAgentThread, written.Thread, nil},
		"text trimmed":                  {request.Text()[1:], OriginAgentThread, written.Thread, nil},
		"a thread with a wake":          {request.Text(), OriginAgentThread, written.Thread, wake},
		"a report of no known kind":     {"n\nd", OriginAgentWake, nil, &OriginWake{Kind: "nonsense", Facts: wake.Facts}},
		"a report with no facts":        {"n\nd", OriginAgentWake, nil, &OriginWake{Kind: "loop"}},
		"a report of no reporter":       {"n\nd", OriginAgentWake, nil, nil},
		"the person's with an agent's":  {"mine", "", written.Thread, nil},
		"an origin the app never wrote": {"x", "peer-session", nil, nil},
	} {
		message, err := DecodeStored(tc.text, tc.origin, tc.thread, tc.wake)
		if !errors.Is(err, ErrStoredMessage) {
			t.Errorf("%s: err %v, want ErrStoredMessage", name, err)
			continue
		}
		if message.Text() != tc.text {
			t.Errorf("%s: text %q, want it as stored", name, message.Text())
		}
		attribution := message.Attribution()
		for _, facts := range []*threadtools.MessageFacts{factsOf(attribution.Thread), factsOfWake(attribution.Wake)} {
			if facts != nil && facts.Form != threadtools.FormWhole {
				t.Errorf("%s: facts %+v kept, want the whole text", name, facts)
			}
		}
	}
	// The stored attribution is the caller's: decoding copies it.
	written.Thread.Title = "changed"
	if read.Attribution().Thread.Title != "Lead" {
		t.Fatal("the decoded message shares its attribution with the caller")
	}
}

func factsOf(thread *OriginThread) *threadtools.MessageFacts {
	if thread == nil {
		return nil
	}
	return thread.Facts
}

func factsOfWake(wake *OriginWake) *threadtools.MessageFacts {
	if wake == nil {
		return nil
	}
	return wake.Facts
}

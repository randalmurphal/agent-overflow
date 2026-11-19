package threadtools

import (
	"errors"
	"strings"
	"testing"
)

// TestCheckRefusesFactsThatDoNotDescribeTheirText holds Check to the facts
// a reader may trust: written for the text's length, of a known form, and
// with body spans inside the text, in order, not empty, not overlapping and
// on character boundaries.
func TestCheckRefusesFactsThatDoNotDescribeTheirText(t *testing.T) {
	text := "héllo, wörld"
	good := MessageFacts{Form: FormMessage, Bytes: len(text), Body: []Span{{Start: 0, End: 6}, {Start: 8, End: len(text)}}}
	if err := good.Check(text); err != nil {
		t.Fatalf("facts that describe the text: %v", err)
	}
	with := func(change func(*MessageFacts)) MessageFacts {
		facts := good.Clone()
		change(&facts)
		return facts
	}
	for name, facts := range map[string]MessageFacts{
		"another length":                      with(func(f *MessageFacts) { f.Bytes-- }),
		"no form":                             with(func(f *MessageFacts) { f.Form = "" }),
		"an unknown form":                     with(func(f *MessageFacts) { f.Form = "note" }),
		"an outcome on a message":             with(func(f *MessageFacts) { f.Outcome = WakeReply }),
		"an answer of no outcome":             with(func(f *MessageFacts) { f.Form = FormAnswer }),
		"an unknown outcome":                  with(func(f *MessageFacts) { f.Form, f.Outcome = FormAnswer, "done" }),
		"a span past the text":                with(func(f *MessageFacts) { f.Body[1].End = len(text) + 1 }),
		"a negative span":                     with(func(f *MessageFacts) { f.Body[0].Start = -1 }),
		"an empty span":                       with(func(f *MessageFacts) { f.Body[0].End = f.Body[0].Start }),
		"spans out of order":                  with(func(f *MessageFacts) { f.Body[0], f.Body[1] = f.Body[1], f.Body[0] }),
		"overlapping spans":                   with(func(f *MessageFacts) { f.Body[1].Start = 5 }),
		"a span starting mid-character":       with(func(f *MessageFacts) { f.Body[0].Start = 2 }),
		"a span ending mid-character":         with(func(f *MessageFacts) { f.Body[0].End = 2 }),
		"a whole message of part of its text": {Form: FormWhole, Bytes: len(text), Body: []Span{{Start: 0, End: 6}}},
	} {
		if err := facts.Check(text); !errors.Is(err, ErrFactsMismatch) {
			t.Errorf("%s: Check = %v, want ErrFactsMismatch", name, err)
		}
	}
	if err := Whole(text).Check(text); err != nil {
		t.Errorf("Whole: %v", err)
	}
	if err := Whole("").Check(""); err != nil {
		t.Errorf("Whole of nothing: %v", err)
	}
}

// TestCheckRefusesARequestWhoseTextWasTrimmed is the reviewer's
// counterexample: a request whose body opens with white space, its text
// trimmed behind its facts' back, no longer reads as its body.
func TestCheckRefusesARequestWhoseTextWasTrimmed(t *testing.T) {
	text, facts := Footer{Title: "Lead", ThreadID: "t-lead", Token: "tok-1", AnswerRequested: true}.Message("\n\n  Build   the docs  \nand more")
	if err := facts.Check(text); err != nil || facts.BodyOf(text) != "Build   the docs  \nand more" {
		t.Fatalf("as written: body %q, %v", facts.BodyOf(text), err)
	}
	if err := facts.Check(strings.TrimSpace(text)); !errors.Is(err, ErrFactsMismatch) {
		t.Fatalf("trimmed: Check = %v, want ErrFactsMismatch", err)
	}
}

// TestCloneSharesNothing holds Clone to a copy a caller can change without
// changing the facts it was made from.
func TestCloneSharesNothing(t *testing.T) {
	role := "Runs the builds."
	facts := MessageFacts{Body: []Span{{Start: 0, End: 1}}, Tokens: []string{"a"}, Role: &role,
		Threads: []FactsThread{{ThreadID: "t", Tokens: []string{"b"}, Role: &role}}}
	copied := facts.Clone()
	copied.Body[0].End, copied.Tokens[0], *copied.Role = 9, "x", "changed"
	copied.Threads[0].Tokens[0], *copied.Threads[0].Role = "y", "changed"
	if facts.Body[0].End != 1 || facts.Tokens[0] != "a" || *facts.Role != role || facts.Threads[0].Tokens[0] != "b" || *facts.Threads[0].Role != role {
		t.Fatalf("the original changed: %+v", facts)
	}
}

// TestPlainMessageIsAllBody holds a plain message's facts to all of its
// words.
func TestPlainMessageIsAllBody(t *testing.T) {
	text, facts := PlainMessage("  Question: Q?\nAnswer: A  \n")
	if text != "  Question: Q?\nAnswer: A  \n" || facts.Form != FormMessage || facts.Check(text) != nil || facts.BodyOf(text) != "Question: Q?\nAnswer: A" {
		t.Fatalf("PlainMessage = %q %+v", text, facts)
	}
}

// TestWorkerStoppedNoticeIsItsBody holds the worker-stopped notice's facts
// to its whole text, the spawn's token and the user's stop.
func TestWorkerStoppedNoticeIsItsBody(t *testing.T) {
	text, facts := WorkerStopped{Title: "W", ThreadID: "t-w", Token: "k", Failures: []string{"its turn."}}.Message()
	if facts.Check(text) != nil || facts.BodyOf(text) != text || facts.Outcome != OutcomeWorkerStopped || facts.StoppedBy != "the user" || len(facts.Tokens) != 1 || facts.Tokens[0] != "k" {
		t.Fatalf("WorkerStopped = %q %+v", text, facts)
	}
	if !strings.Contains(text, `thread "W" (t-w)`) || !strings.Contains(text, "may still be running or queued: its turn.") {
		t.Fatalf("text %q", text)
	}
}

package threadtools

import (
	"strings"
	"testing"
)

// TestCauseAnswerKeepsItsWords holds each cause's answer, the words
// thread_status and a paired computer's older build read, to the sentence
// the settlement stored before the cause was data.
func TestCauseAnswerKeepsItsWords(t *testing.T) {
	for _, tc := range []struct {
		cause   Cause
		written string
		want    string
	}{
		{Cause{Code: CauseRestart}, "", "This computer restarted before the request finished."},
		{Cause{Code: CauseSessionEndedUnread}, "", "The thread's session ended before it read the request."},
		{Cause{Code: CauseSessionEndedUnanswered}, "", "The thread's session ended before it answered the request."},
		{Cause{Code: CauseSessionEndedMidTurn}, "", "The thread's session ended during the turn that read the request."},
		{Cause{Code: CauseSessionEndedMidTurn}, "Half done.", "The thread's session ended during the turn that read the request. What it had written by then:\n\nHalf done."},
		{Cause{Code: CauseSessionEndedWorking}, "", "The thread's session ended while it was still working on the request."},
		{Cause{Code: CauseSessionEndedWorking}, "Half done.", "The thread's session ended while it was still working on the request. What it had written by then:\n\nHalf done."},
		{Cause{Code: CauseTurn}, "", "The turn was interrupted before it answered."},
		{Cause{Code: CauseNeverStarted}, "", "That request was accepted but its thread was never created."},
		{Cause{Code: CauseNotTakenIn}, "", "The thread did not take the request in."},
		{Cause{Code: CauseNotTakenInToComposer}, "", "The thread did not take the request in, and its message went back to that thread's composer."},
		{Cause{Code: CauseHandedOff, Arg: "t-old"}, "", "The assistant moved to a new thread before it answered. Its previous thread is t-old; read it with thread_show to see what was done, or ask the assistant again by name."},
		{Cause{Code: CauseMoved, Arg: "Desk (Linux)"}, "", "The thread was moved to Desk (Linux) before it answered."},
		{Cause{Code: CauseMoved}, "", "The thread was moved to another computer before it answered."},
		{Cause{Code: CauseThreadCancel, Arg: `"Lead" (t-lead, on desk)`}, "", `"Lead" (t-lead, on desk) stopped its turn with thread_cancel before it answered.`},
		{Cause{Code: CauseTrayStop}, "", "The user stopped its turn from the Agents tray before it answered."},
		{Cause{Code: CauseTakeover}, "", "The user took the thread over from its workflow before it answered."},
		{Cause{Code: CauseWorkflow}, "", "Its workflow stopped the turn before it answered."},
		{Cause{Code: CauseAssistantsOff}, "", "Assistants were turned off in Settings before it answered."},
		// No cause: an earlier build's row or a computer that reports none
		// answers with its text alone.
		{Cause{}, "This computer restarted before the request finished.", "This computer restarted before the request finished."},
	} {
		if got := tc.cause.Answer(tc.written); got != tc.want {
			t.Errorf("%+v with %q answers\n %q\nwant\n %q", tc.cause, tc.written, got, tc.want)
		}
	}
}

// TestCauseVocabularyIsClosed holds Known to the codes the renderer words,
// and nothing else: a code from a newer computer is no cause here.
func TestCauseVocabularyIsClosed(t *testing.T) {
	for _, code := range causeCodes {
		if !(Cause{Code: code}).Known() {
			t.Errorf("%s is in the vocabulary and is not known", code)
		}
		if strings.TrimSpace(Cause{Code: code}.Answer("")) == "" {
			t.Errorf("%s has no words", code)
		}
	}
	for _, code := range []string{"", "Restart", "restart ", "evicted"} {
		if (Cause{Code: code}).Known() {
			t.Errorf("%q is known", code)
		}
	}
}

// TestWakeOfACauseFromAnotherWording reads a cause whatever its text: a
// paired computer that words a sentence differently still reports the
// same code, and the wake says what this build says for it.
func TestWakeOfACauseFromAnotherWording(t *testing.T) {
	sources := []WakeSource{{Title: "Worker", ThreadID: "t-work", Computer: "desk", Tokens: []string{"tok"}}}
	text, facts := Wake{Kind: WakeInterrupted, Sources: sources, Cause: Cause{Code: CauseMoved, Arg: "Desk"}}.Message()
	if want := `"Worker" (t-work, on desk) [token tok]: The thread was moved to Desk before it answered.`; text != want {
		t.Fatalf("wake = %q, want %q", text, want)
	}
	if facts.StoppedBy != "a move to Desk" || len(facts.Body) != 0 {
		t.Fatalf("facts = %+v, want stopped by a move to Desk with no body", facts)
	}
}

// TestWakeWithNoCauseShowsItsTextWhole reads a request with no cause, one
// an earlier build settled or a computer reported without one, as its text:
// all of it is the message, clipped as an answer, and nothing names a
// stopper, even when the text is one of this build's sentences.
func TestWakeWithNoCauseShowsItsTextWhole(t *testing.T) {
	sources := []WakeSource{{Title: "Worker", ThreadID: "t-work", Tokens: []string{"tok"}}}
	for _, reason := range []string{
		"This computer restarted before the request finished.",
		"The thread's session ended during the turn that read the request. What it had written by then:\n\nHalf done.",
	} {
		text, facts := Wake{Kind: WakeInterrupted, Sources: sources, Text: reason}.Message()
		if want := `"Worker" (t-work) [token tok]: ` + reason; text != want {
			t.Errorf("wake = %q, want %q", text, want)
		}
		if facts.StoppedBy != "" || facts.BodyOf(text) != reason || facts.Check(text) != nil {
			t.Errorf("facts = %+v, want the whole reason as the body and no stopper", facts)
		}
	}
	long := strings.Repeat("x", WakePreviewBytes+10)
	text, facts := Wake{Kind: WakeInterrupted, Sources: sources, Text: long}.Message()
	if !facts.Clipped || !strings.HasSuffix(text, WakePreviewPointer("tok")) {
		t.Errorf("a long text with no cause is not clipped as an answer: %+v", facts)
	}
}

// TestWakeOfAWrittenTurnClipsOnlyWhatItWrote clips what a turn whose
// session ended had written as an answer, the sentence before it whole.
func TestWakeOfAWrittenTurnClipsOnlyWhatItWrote(t *testing.T) {
	sources := []WakeSource{{Title: "Worker", ThreadID: "t-work", Tokens: []string{"tok"}}}
	written := strings.Repeat("y", WakePreviewBytes+10)
	text, facts := Wake{Kind: WakeInterrupted, Sources: sources, Cause: Cause{Code: CauseSessionEndedMidTurn}, Text: written}.Message()
	opening := `"Worker" (t-work) [token tok]: The thread's session ended during the turn that read the request. What it had written by then:` + "\n\n"
	if !strings.HasPrefix(text, opening) || !facts.Clipped {
		t.Fatalf("wake = %.200q, facts %+v", text, facts)
	}
	if got := facts.BodyOf(text); got != written[:WakePreviewBytes] {
		t.Fatalf("body is %d bytes, want the first %d of what it wrote", len(got), WakePreviewBytes)
	}
}

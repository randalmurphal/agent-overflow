package threadtools

import "slices"

// Cause is why an interrupted request was interrupted, recorded where its
// settlement decides it: a code from the closed vocabulary below and the one
// argument some codes carry. What the turn had written by then is the
// request's answer text beside it, never part of the cause. The receipt and
// the request store it, a paired computer reports it beside the text, and
// every reader renders words from it (Answer, Wake.Message); nothing reads a
// cause back out of words, or out of the answer's kind.
//
// A request with no known code, one an earlier build settled or a computer
// reported without one or with one this build does not know, carries only
// its text: it is shown whole and names no stopper, whatever its kind.
type Cause struct {
	Code string `json:"code"`
	Arg  string `json:"arg,omitempty"`
}

// The causes of an interrupted request.
const (
	// CauseRestart: the computer that held the request restarted.
	CauseRestart = "restart"
	// CauseSessionEndedUnread: the thread's session ended, by a crash, a
	// stop, a teardown or a replacement, before the provider took the
	// request's message in.
	CauseSessionEndedUnread = "session_ended_unread"
	// CauseSessionEndedUnanswered: the session ended after the provider
	// took the message in and before the turn that reads it opened.
	CauseSessionEndedUnanswered = "session_ended_unanswered"
	// CauseSessionEndedMidTurn: the session ended during the turn that read
	// the message. The answer text is what that turn had written by then.
	CauseSessionEndedMidTurn = "session_ended_mid_turn"
	// CauseSessionEndedWorking: the turn that read the message ended and the
	// request waited for its thread to come to rest (a spawn, or a send to a
	// thread its caller spawned); the session ended during a later turn. The
	// answer text is what the thread wrote from the request's turn on.
	CauseSessionEndedWorking = "session_ended_working"
	// CauseTurn: the turn was interrupted and no stop was recorded for it.
	CauseTurn = "turn"
	// CauseNeverStarted: the request was accepted and its thread was never
	// created.
	CauseNeverStarted = "never_started"
	// CauseNotTakenIn: the provider gave the request's message back before
	// taking it in, and the request could not go back to its queue.
	CauseNotTakenIn = "not_taken_in"
	// CauseNotTakenInToComposer: the message was not taken in and was joined
	// with the person's own text it could not be split from, so all of it
	// went back to the target thread's composer.
	CauseNotTakenInToComposer = "not_taken_in_to_composer"
	// CauseHandedOff: the thread, an assistant's, was replaced by New
	// Thread, Same Assistant after it read the request and before it
	// answered. Arg is the previous thread's id.
	CauseHandedOff = "handed_off"
	// CauseMoved: the thread was moved to another computer before it
	// answered. Arg names that computer, empty when it is unknown.
	CauseMoved = "moved"
	// CauseThreadCancel: another thread's thread_cancel stopped the turn.
	// Arg is that thread as its sentence names it: its quoted title and its
	// id, with its computer when it is another one.
	CauseThreadCancel = "thread_cancel"
	// CauseTrayStop: the user stopped the turn from the Agents tray.
	CauseTrayStop = "tray_stop"
	// CauseTakeover: the user took the thread over from its workflow.
	CauseTakeover = "takeover"
	// CauseWorkflow: the thread's workflow ended its attempt.
	CauseWorkflow = "workflow"
	// CauseAssistantsOff: assistants were turned off in Settings.
	CauseAssistantsOff = "assistants_off"
	// CauseUserStop: the user stopped the turn with the Stop in its own
	// thread. The answer text is what the turn had said by then, its
	// `final` message, and is all the request answers with as text
	// (Answer).
	CauseUserStop = "user_stop"
)

// causeCodes is the vocabulary, every code render words.
var causeCodes = []string{
	CauseRestart, CauseSessionEndedUnread, CauseSessionEndedUnanswered, CauseSessionEndedMidTurn,
	CauseSessionEndedWorking, CauseTurn, CauseNeverStarted, CauseNotTakenIn, CauseNotTakenInToComposer,
	CauseHandedOff, CauseMoved, CauseThreadCancel, CauseTrayStop, CauseTakeover, CauseWorkflow,
	CauseAssistantsOff, CauseUserStop,
}

// Known reports whether c's code is in this build's vocabulary. A code a
// newer computer reports and this one does not know is no cause here.
func (c Cause) Known() bool { return slices.Contains(causeCodes, c.Code) }

// shownCause is a cause rendered for the answer text it carries.
type shownCause struct {
	// reason is said before the answer, whole: never clipped, and not the
	// message's body.
	reason string
	// answer is the message's body, clipped to the preview as an answer:
	// what the turn had written, the part of a sentence a row shows as the
	// message, or with no known cause the whole text.
	answer string
	// stoppedBy is what stopped the request as a row names it
	// (MessageFacts.StoppedBy); empty when the reason names no stopper.
	stoppedBy string
}

// render is how c reads with text, the request's answer text. This is the
// one place a cause's words are defined. A restart's wake names the
// computer that restarted instead (Wake.status, Wake.digest).
func (c Cause) render(text string) shownCause {
	sessionEnded := func(how string, written bool) shownCause {
		shown := shownCause{reason: "The thread's session ended " + how + ".", stoppedBy: "the session ended " + how}
		if written && text != "" {
			shown.reason += SessionEndedWrittenSoFar
			shown.answer = text
		}
		return shown
	}
	// stoppedIn is a sentence that names who stopped the turn in the
	// stopper's own words, which the row shows as the message.
	stoppedIn := func(sentence string) shownCause { return shownCause{answer: sentence} }
	switch c.Code {
	case CauseRestart:
		return shownCause{reason: "This computer restarted before the request finished."}
	case CauseSessionEndedUnread:
		return sessionEnded("before it read the request", false)
	case CauseSessionEndedUnanswered:
		return sessionEnded("before it answered the request", false)
	case CauseSessionEndedMidTurn:
		return sessionEnded("during the turn that read the request", true)
	case CauseSessionEndedWorking:
		return sessionEnded("while it was still working on the request", true)
	case CauseTurn:
		return shownCause{reason: "The turn was interrupted before it answered.", stoppedBy: "an interrupt"}
	case CauseNeverStarted:
		return stoppedIn("That request was accepted but its thread was never created.")
	case CauseNotTakenIn:
		return shownCause{reason: "The thread did not take the request in.", stoppedBy: "the thread did not take it in"}
	case CauseNotTakenInToComposer:
		return shownCause{
			reason:    "The thread did not take the request in, and its message went back to that thread's composer.",
			stoppedBy: "the thread did not take it in, and its message went back to that thread's composer",
		}
	case CauseHandedOff:
		// The rest names the previous thread, for the sender to read.
		return shownCause{
			reason: "The assistant moved to a new thread before it answered. ",
			answer: "Its previous thread is " + c.Arg +
				"; read it with thread_show to see what was done, or ask the assistant again by name.",
			stoppedBy: "the assistant moving to a new thread",
		}
	case CauseMoved:
		destination := c.Arg
		if destination == "" {
			destination = "another computer"
		}
		return shownCause{reason: "The thread was moved to " + destination + " before it answered.", stoppedBy: "a move to " + destination}
	case CauseThreadCancel:
		return stoppedIn(c.Arg + " stopped its turn with thread_cancel before it answered.")
	case CauseTrayStop:
		return stoppedIn("The user stopped its turn from the Agents tray before it answered.")
	case CauseTakeover:
		return stoppedIn("The user took the thread over from its workflow before it answered.")
	case CauseWorkflow:
		return stoppedIn("Its workflow stopped the turn before it answered.")
	case CauseAssistantsOff:
		return stoppedIn("Assistants were turned off in Settings before it answered.")
	case CauseUserStop:
		shown := shownCause{reason: WakePersonStopped, stoppedBy: "the user"}
		if text != "" {
			shown.reason += " Its last message:\n"
			shown.answer = text
		}
		return shown
	}
	return shownCause{answer: text}
}

// SessionEndedWrittenSoFar introduces, after a session-ended cause's
// sentence, what the turn had written when its session ended.
const SessionEndedWrittenSoFar = " What it had written by then:\n\n"

// Answer is the words an interrupted request answers with where an agent
// reads its answer as text (thread_status, a written answer file, a paired
// computer that does not read causes): the cause's words, followed by what
// the turn had written when the cause carries it. The user's Stop answers
// with what the turn said alone, which its state and `final` kind frame.
// With no known cause it is the text alone.
func (c Cause) Answer(text string) string {
	if c.Code == CauseUserStop {
		return text
	}
	shown := c.render(text)
	return shown.reason + shown.answer
}

// WakePersonStopped is the status of a request whose turn the user's Stop
// in its thread cut before it answered.
const WakePersonStopped = "The user stopped this turn before it answered."

// WakeDigestPersonStopped follows the count on a digest of requests whose
// turns the user's Stop cut, the plural of WakePersonStopped.
const WakeDigestPersonStopped = "The user stopped their turns before they answered."

package threadtools

import (
	"fmt"
	"log"
	"strings"

	"agent-overflow/internal/errorsx"
)

// Error codes. Every refusal the model can act on has a stable code and
// prose that names the operation and the ids to retry with, following the
// remote-commands error shape. Raw causes stay in host logs.
const (
	// CodeInvalidRequest is an argument this package refused before any
	// work started.
	CodeInvalidRequest = "thread_invalid_request"
	// CodeNotFound is a reference no computer matched.
	CodeNotFound = "thread_not_found"
	// CodeAmbiguous is a prefix more than one thread matched.
	CodeAmbiguous = "thread_ambiguous"
	// CodeResolutionIncomplete is a fan-out where a computer did not
	// answer in time and nothing matched elsewhere. Never a confident
	// not found.
	CodeResolutionIncomplete = "thread_resolution_incomplete"
	// CodeSelfSend is a thread sending to or asking itself.
	CodeSelfSend = "thread_self_send"
	// CodeIsCaller is the calling thread refusing to archive itself.
	CodeIsCaller = "thread_is_caller"
	// CodeGrouped is a pin on a thread whose group carries the pin.
	CodeGrouped = "thread_grouped"
	// CodeNotYours is a thread the caller never spawned, sent to or
	// asked.
	CodeNotYours = "thread_not_yours"
	// CodeRequestUnknown is a token no request record holds.
	CodeRequestUnknown = "thread_request_unknown"
	// CodeRequestNotYours is a token that belongs to another thread. The
	// token alone is not a capability.
	CodeRequestNotYours = "thread_request_not_yours"
	// CodeUnsupported is a destination too old for these tools.
	CodeUnsupported = "thread_unsupported"
	// CodeUnreachable is a paired computer that could not be reached for
	// a call that names it directly.
	CodeUnreachable = "thread_computer_unreachable"
)

func invalidf(format string, args ...any) error {
	return errorsx.Public(CodeInvalidRequest, fmt.Sprintf(format, args...), nil)
}

func publicf(code, format string, args ...any) error {
	return errorsx.Public(code, fmt.Sprintf(format, args...), nil)
}

// notFound names the reference and says how to find the right one, since a
// model that guessed an id has to be sent back to a result it can trust.
func notFound(ref string, searched []Computer) error {
	message := fmt.Sprintf("No thread matches %q on this computer.", ref)
	if len(searched) > 0 {
		message = fmt.Sprintf("No thread matches %q on this computer or on %s.", ref, computerList(searched))
	}
	return errorsx.Public(CodeNotFound, message+" Find the thread with thread_search and use the thread_id from the result; never guess an id.", nil)
}

// ambiguous lists every candidate with its computer so the next call can
// name one exactly.
func ambiguous(ref string, candidates []Candidate, paired bool) error {
	parts := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		part := fmt.Sprintf("%s (%q)", candidate.ThreadID, candidate.Title)
		if paired && candidate.Computer != "" {
			part += " on " + candidate.Computer
		}
		parts = append(parts, part)
	}
	return errorsx.Public(CodeAmbiguous, fmt.Sprintf("%q matches %d threads: %s. Call again with one full thread_id.", ref, len(candidates), strings.Join(parts, "; ")), nil)
}

// resolutionIncomplete is what a fan-out returns instead of a confident
// not found when a computer did not answer.
func resolutionIncomplete(ref string, silent []Computer) error {
	return errorsx.Public(CodeResolutionIncomplete, fmt.Sprintf("No thread matches %q on the computers that answered, and %s did not answer in time. The thread may be there. Try again, or name the computer with computer_id.", ref, computerList(silent)), nil)
}

func computerList(computers []Computer) string {
	names := make([]string, 0, len(computers))
	for _, computer := range computers {
		names = append(names, NameOfComputer(computer))
	}
	switch len(names) {
	case 0:
		return "no computer"
	case 1:
		return names[0]
	case 2:
		return names[0] + " and " + names[1]
	default:
		return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
	}
}

// publicMessage returns the reviewed prose of err. Result rows that report
// a per-item failure use it, and those rows reach the model, so an error
// that carries no reviewed prose becomes a fixed refusal and its own text
// stays in the host log: a raw cause can name a path, a query or an id the
// model has no business reading.
func publicMessage(err error) (code, message string) {
	if err == nil {
		return "", ""
	}
	if code, message, ok := errorsx.PublicDetails(err); ok {
		return code, message
	}
	log.Printf("thread tools: %v", err)
	return CodeInvalidRequest, "Thread tools could not complete that part of the call. The cause is in this computer's log."
}

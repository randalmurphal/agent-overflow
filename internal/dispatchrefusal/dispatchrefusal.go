// Package dispatchrefusal words the refusals a computer's dispatcher gives
// before any method runs: a call it does not have (method_not_found) and a
// call whose arguments it cannot read (bad_params). Both mean the computer
// did not run the call, and both usually mean the caller was built from a
// different release.
//
// The dispatcher sends these sentences as the refusal's message
// (internal/transport), so a reader already shipped shows them as they are.
// Every Go reader of a refusal (rpcclient, the Windows launcher's client,
// the harness client) reads it through Message, so a computer that still
// sends the old wire text reads the same. The frontend has the same two
// sentences (frontend/src/lib/transport/dispatchRefusal.ts), pinned to
// these by this package's tests.
//
// A dispatcher refusal carries no diagnostic detail; a failure a method
// returned always does (transport.CallerError). That is what tells the
// two apart when a method forwards another computer's refusal under the
// same code. A bad_params refusal for one argument that did not decode
// carries detail too, and its message is already Unreadable.
//
// Leaf package: it imports nothing, so a client that must not link the
// transport server can still use it.
package dispatchrefusal

// The wire codes, mirroring transport.ErrCodeMethodNotFound and
// transport.ErrCodeBadParams (pinned by internal/transport's tests).
const (
	CodeMethodNotFound = "method_not_found"
	CodeBadParams      = "bad_params"
)

// Unsupported is what a call the computer does not have reads as.
const Unsupported = "The computer this reached does not support this action. It may run a different version of Agent Overflow. If so, update the older one, or reload a page left open during an update."

// Unreadable is what a call whose arguments the computer could not read
// reads as.
const Unreadable = "The computer this reached could not read this request. It may run a different version of Agent Overflow. If so, update the older one, or reload a page left open during an update."

// Is reports whether a refusal with code is the answering dispatcher's own:
// detailed says whether it carried the diagnostic detail a method's failure
// carries.
func Is(code string, detailed bool) bool {
	return !detailed && (code == CodeMethodNotFound || code == CodeBadParams)
}

// Message is what a person reads for a refusal: the sentence for the
// dispatcher's own refusal, else the refusal's message.
func Message(code, message string, detailed bool) string {
	if !Is(code, detailed) {
		return message
	}
	if code == CodeMethodNotFound {
		return Unsupported
	}
	return Unreadable
}

// Detailed reports whether a refusal's raw "detail" field, as a reader
// that does not decode it holds it, is there at all.
func Detailed(raw []byte) bool {
	return len(raw) > 0 && string(raw) != "null"
}

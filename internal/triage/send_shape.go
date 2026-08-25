package triage

import (
	"fmt"
	"strings"
	"testing"
)

// sendShape names the grammar class of an AO-initiated send. It is
// stamped once, at registration, by the registrar the send path chose;
// there is no derivation from the id and no default reachable by
// omission, because `registerPendingSend` takes it as a parameter and
// every public registrar passes exactly one value. The stamp is the
// AUTHORITATIVE flush classifier — the readers that used to sniff the
// id for ":flush:" now branch on `Shape == sendShapeFlush`.
//
// The three shapes are the three id grammars the App layer allocates:
//
//	sendShapeDirect  user:<turn>                  (app_send.go)
//	sendShapeFlush   user:<turn>:flush:<n>        (app_flush_queue.go)
//	sendShapeSteer   user:<turn>:steer:<n>        (app_steer.go)
//
// The id grammar is minted in the App layer (`nextFlushUserItemID`, the
// one `user:%d:flush:%d` allocator in app_flush_queue.go), so the stamp
// and the grammar ship as one binary but could drift if a send path
// registered through the wrong registrar. assertSendShapeMatchesID is
// the permanent tripwire against that: it panics in any test binary,
// and every production registration site is covered by the root suite,
// so a mis-chosen registrar fails CI at the surface that chose it.
type sendShape uint8

const (
	// sendShapeDirect is a turn-opening send: the row that IS the turn's
	// first user message, already at its final timeline position.
	sendShapeDirect sendShape = iota

	// sendShapeFlush is a queued send dispatched off AO's own flush
	// queue — the only shape whose row may still need repositioning at
	// echo time, which is what the classification exists to answer.
	sendShapeFlush

	// sendShapeSteer is a mid-turn Codex steer. Like a direct send it is
	// already at its intended slot and must never be repositioned.
	sendShapeSteer
)

func (s sendShape) String() string {
	switch s {
	case sendShapeDirect:
		return "direct"
	case sendShapeFlush:
		return "flush"
	case sendShapeSteer:
		return "steer"
	default:
		return fmt.Sprintf("sendShape(%d)", uint8(s))
	}
}

// assertSendShapeMatchesID panics (test binaries only) when a
// registered send's stamped shape contradicts its id grammar. Both
// values are AO-authored in the same registerPendingSend call and
// immutable afterwards, so registration is the ONLY moment a
// disagreement can be born — a wire echo can never create one. One
// exception is deliberate and encoded here: a flush row registered
// through the direct surface (the Codex post-interrupt re-send) is
// flush-shaped despite carrying no queue item id, which is why
// RegisterPendingFlushResendWithExpectation exists rather than a
// grammar-inference rule.
//
// Production pays one boolean check and never panics: the shape is
// already the decision everywhere, so production drift would mean a
// registrar bug that this assertion's CI coverage exists to catch
// before release.
func assertSendShapeMatchesID(threadID, aoItemID string, shape sendShape) {
	if !testing.Testing() {
		return
	}
	if strings.Contains(aoItemID, ":flush:") != (shape == sendShapeFlush) {
		panic(fmt.Sprintf(
			"triage: send-shape mismatch at registration: thread=%s aoItemID=%q stamped=%s — the registrar chose the wrong shape",
			threadID, aoItemID, shape,
		))
	}
}

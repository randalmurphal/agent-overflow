package triage

import (
	"fmt"
	"log"
	"strings"
	"sync"
	"testing"
)

// send_shape.go carries the TYPED answer to a question six sites in this
// package currently answer by sniffing an id for the ":flush:"
// substring, plus the drift assertion that keeps the two honest while
// both exist.
//
// The sniff stays AUTHORITATIVE this release. Nothing reads sendShape to
// make a decision — every call site still branches on the substring and
// the typed field only gets compared against it. The field is here to
// soak: once a release has shipped with no drift reported, the sniff
// sites become `entry.Shape == sendShapeFlush` and this file's
// comparison helpers go away with them.
//
// Why the sniff cannot simply be replaced today: the id grammar is
// minted in the App layer (`nextFlushUserItemID`, the one `user:%d:flush:%d`
// allocator in app_flush_queue.go) and read here, so the two halves ship
// as one binary but were never pinned to each other. A silent
// disagreement misplaces a queued user message in the timeline — the
// failure class this subsystem has spent fourteen review rounds on — so
// the new field earns trust by agreeing, not by being believed.

// sendShape names the grammar class of an AO-initiated send. It is
// stamped once, at registration, by the registrar the send path chose;
// there is no derivation from the id and no default reachable by
// omission, because `registerPendingSend` takes it as a parameter and
// every public registrar passes exactly one value.
//
// The three shapes are the three id grammars the App layer allocates:
//
//	sendShapeDirect  user:<turn>                  (app_send.go)
//	sendShapeFlush   user:<turn>:flush:<n>        (app_flush_queue.go)
//	sendShapeSteer   user:<turn>:steer:<n>        (app_steer.go)
//
// Note that the sniff can only ever distinguish flush from not-flush, so
// the drift assertion below pins that axis alone. Direct-vs-steer is
// information the substring never carried; it exists here because the
// registration surface always knew it and throwing it away is what
// forced the sniff in the first place.
type sendShape uint8

const (
	// sendShapeDirect is a turn-opening send: the row that IS the turn's
	// first user message, already at its final timeline position.
	sendShapeDirect sendShape = iota

	// sendShapeFlush is a queued send dispatched off AO's own flush
	// queue — the only shape whose row may still need repositioning at
	// echo time, which is exactly why the sniff exists.
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

// The disagreement sites, named so a production log line says which
// decision was about to be made on a drifting entry. Each constant is
// used exactly once; the set doubles as the checklist for the eventual
// sniff deletion.
const (
	sendShapeSiteRegister         = "register"
	sendShapeSiteDeferredCount    = "deferred-pending-flush-count"
	sendShapeSiteMaxFlushSequence = "max-pending-flush-sequence"
	sendShapeSiteDrainUnconfirmed = "drain-unconfirmed-flush-items"
	sendShapeSiteLiveSnapshot     = "live-state-snapshot"
	sendShapeSiteEchoEagerBump    = "echo-unanchored-eager-bump"
	sendShapeSitePromoteQuiet     = "promote-quiet-flush-sends"
	sendShapeSiteRebumpSiblings   = "rebump-anchored-quiet-siblings"
)

// flushIDSniff is the legacy classifier, extracted verbatim so the six
// call sites and the assertion cannot drift from EACH OTHER while the
// substring is still the answer.
func flushIDSniff(aoItemID string) bool {
	return strings.Contains(aoItemID, ":flush:")
}

// sniffFlushShape returns the SNIFF's verdict — unchanged, authoritative,
// the value the caller must branch on — and reports a disagreement with
// the shape stamped at registration.
//
// Callers hold r.mu at most of these sites, so the drift bookkeeping
// deliberately uses its own mutex and never touches router state.
func (r *Router) sniffFlushShape(threadID string, entry *pendingSend, site string) bool {
	sniffed := flushIDSniff(entry.AOItemID)
	if sniffed != (entry.Shape == sendShapeFlush) {
		r.reportSendShapeDrift(site, threadID, entry.AOItemID, sniffed, entry.Shape)
	}
	return sniffed
}

// reportSendShapeDrift is loud in a test binary and bounded in
// production.
//
// A test binary PANICS: a drifting stamp means a send path registered
// through the wrong registrar, and the whole point of the soak is that
// the disagreement is impossible to ignore while the sniff still covers
// for it. Production logs ONE line per site — the sniff is still making
// the decision, so a per-entry log would be pure noise on a thread that
// keeps sending, and the first line already names the id, both verdicts,
// and the decision that was about to be made.
func (r *Router) reportSendShapeDrift(site, threadID, aoItemID string, sniffed bool, stamped sendShape) {
	sniffLabel := "not-flush"
	if sniffed {
		sniffLabel = "flush"
	}
	msg := fmt.Sprintf(
		"triage: send-shape drift at %s: thread=%s aoItemID=%q sniff=%s stamped=%s — the sniff stays authoritative; fix the registrar",
		site, threadID, aoItemID, sniffLabel, stamped,
	)
	if testing.Testing() {
		panic(msg)
	}
	r.sendShapeDriftMu.Lock()
	if r.sendShapeDriftLogged == nil {
		// One entry per site constant above; the map cannot grow past
		// that set, so it needs no cap-and-reset like the session-status
		// throttle.
		r.sendShapeDriftLogged = make(map[string]struct{}, 8)
	}
	if _, seen := r.sendShapeDriftLogged[site]; seen {
		r.sendShapeDriftMu.Unlock()
		return
	}
	r.sendShapeDriftLogged[site] = struct{}{}
	r.sendShapeDriftMu.Unlock()
	log.Print(msg)
}

// sendShapeDrift is the Router-side state this file owns: its own mutex
// so a check can run at a site that already holds r.mu.
type sendShapeDrift struct {
	sendShapeDriftMu     sync.Mutex
	sendShapeDriftLogged map[string]struct{}
}

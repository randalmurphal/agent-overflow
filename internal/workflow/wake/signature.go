package wake

import (
	"fmt"
	"strconv"
	"strings"

	"agent-overflow/internal/untrustedtext"
)

// A wake's signature: what makes two wakes THE SAME ASK.
//
// Deduplication is by content, never by a time window. A timer answers the
// wrong question in both directions — it suppresses a genuinely new state that
// arrives quickly (a wave that parks twice in a minute for two different
// reasons) and lets a duplicate through that arrives slowly (a run re-parked
// identically an hour later). What a reader actually experiences as a duplicate
// is a message that says what the last one said, so that is what the signature
// is over.
//
// The rule for what belongs here is exactly "does it change the message the
// reader sees". Every field below appears in the composed text, and every
// free-text field is bounded HERE the way the composer bounds it THERE: two
// causes that differ only past `MaxCauseRunes` produce byte-identical messages,
// so treating them as different asks would deliver the same words twice.
//
// The signature is a readable string rather than a hash because it is persisted
// on the run row and read by a human debugging a wake that did or did not
// arrive; a 64-bit opaque number would answer none of the questions that reader
// has.

// signature field separator. Every value is quoted before it is joined, so no
// value can forge a field boundary.
const signatureSeparator = " "

// Signature identifies the ask a RESTING wake carries. Fields: which run,
// where it came to rest, the coordinate it rested on, and — for a descendant
// park — which descendant and what stopped it.
//
// Note what is deliberately NOT here: the declared outputs, the references, the
// workspace, and the attempt digest. All of them are derived from the
// coordinate above, so a run resting twice at the same coordinate with a
// different digest has not been asked a second question; it has had its record
// re-read.
func Signature(in Input) string {
	fields := []string{
		"kind=rest",
		field("run", in.Run.ItemID),
		field("state", in.Run.State),
		field("reason", in.Run.Reason),
		field("phase", in.Run.PhaseID),
		"attempt=" + strconv.Itoa(in.Run.Attempt),
		field("detail", untrustedtext.Truncate(strings.TrimSpace(in.Run.Detail), MaxDetailRunes)),
		field("cause", untrustedtext.Truncate(strings.TrimSpace(in.Run.Cause), MaxCauseRunes)),
		field("gate", in.Run.GateDecision),
	}
	if child := in.Descendant; child != nil {
		fields = append(fields,
			field("child", child.ItemID),
			field("child-state", child.State),
			field("child-reason", child.Reason),
			field("child-phase", child.PhaseID),
			"child-attempt="+strconv.Itoa(child.Attempt),
			field("child-detail", untrustedtext.Truncate(strings.TrimSpace(child.Detail), MaxDetailRunes)),
			field("child-cause", untrustedtext.Truncate(strings.TrimSpace(child.Cause), MaxCauseRunes)),
			field("child-gate", child.GateDecision),
		)
	}
	return strings.Join(fields, signatureSeparator)
}

// ProgressSignature identifies the ask a PROGRESS wake carries. The attempt is
// load-bearing and not optional: a campaign's loop-back notify fires once per
// wave over the same phase and the same route, and a signature without the
// attempt would report wave 1 and silently swallow every wave after it — which
// is the exact failure this whole mechanism exists to prevent, inverted.
func ProgressSignature(in ProgressInput) string {
	return strings.Join([]string{
		"kind=progress",
		field("run", in.Run.ItemID),
		field("gate-run", in.Gate.ItemID),
		field("phase", in.Gate.PhaseID),
		"attempt=" + strconv.Itoa(in.Gate.Attempt),
		field("decision", in.Gate.Decision),
		field("target", in.Gate.Target),
	}, signatureSeparator)
}

func field(name, value string) string {
	return fmt.Sprintf("%s=%s", name, strconv.Quote(value))
}

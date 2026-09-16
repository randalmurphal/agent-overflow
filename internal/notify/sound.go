package notify

// Notification SOUNDS. A cue is a second presentation of a notification that
// has already been decided — it is not a fourth kind of send and it has no
// decision of its own. `App.notifyOS` is the one gate (per-kind toggle,
// hidden-thread opt-in, attended screen); a cue is published only where that
// gate let a banner through, which is what makes "a sound never fires where a
// banner would be suppressed" structural rather than remembered.
//
// The payload is deliberately CONTENTLESS. A banner may name a thread; a
// sound says nothing, so there is nothing here to redact and nothing a
// listening client could read off the channel that it could not already.

// SoundEvent is the reaction a cue is for. Three, not six: the kinds answer
// three questions a person acts on differently — the agent finished, the
// agent needs you, something is wrong — and a cue picker per notify.Kind
// would be four pickers for a distinction nobody makes by ear.
type SoundEvent string

const (
	// SoundTurnComplete is the agent finishing and the thread resting.
	SoundTurnComplete SoundEvent = "turn-complete"
	// SoundInputNeeded is work stopped until a person acts.
	SoundInputNeeded SoundEvent = "input-needed"
	// SoundAttention is a failure or a system that needs a hand: a failed
	// turn, a dead provider, a signed-out login, a parked workflow, an
	// update that did not apply.
	SoundAttention SoundEvent = "attention"
)

// soundEvents maps every Kind onto its event. TOTAL over `kinds`, and
// TestSoundEventCoversEveryKind fails when a new kind arrives without one: an
// unmapped kind would raise a banner with no cue and no setting to explain
// why, which reads as a broken speaker rather than a preference.
var soundEvents = map[Kind]SoundEvent{
	KindTurnComplete:      SoundTurnComplete,
	KindApprovalNeeded:    SoundInputNeeded,
	KindError:             SoundAttention,
	KindProviderSignedOut: SoundAttention,
	KindWorkflowAttention: SoundAttention,
	KindAppUpdate:         SoundAttention,
}

// SoundEventFor answers which cue event a kind belongs to. An undeclared kind
// answers "" and false; ValidateSend has already refused one.
func SoundEventFor(kind Kind) (SoundEvent, bool) {
	event, ok := soundEvents[kind]
	return event, ok
}

// SoundCue is the wire payload: which cue to play, and which event asked for
// it. The cue is resolved host-side from the backend screen's settings, so a
// presenter plays what it is told rather than re-deriving a preference.
type SoundCue struct {
	// Event is the moment class, for a client that wants to log or gate on
	// it. It carries no thread, no title and no text.
	Event SoundEvent `json:"event"`
	// Cue names one of the built-in cue assets (settings.NotifyCue*). A
	// player that does not recognise it plays nothing and says so in its
	// debug log rather than substituting another sound.
	Cue string `json:"cue"`
}

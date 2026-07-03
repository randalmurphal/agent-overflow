package discussion

import "sync"

// DefaultMaxTurns is the circuit-breaker turn count a new deliberation
// falls back to when the caller supplies a non-positive value.
const DefaultMaxTurns = 8

// DeliberationState tracks in-memory turn state for an active discussion.
type DeliberationState struct {
	ChannelID      string `json:"channelId"`
	CurrentSpeaker string `json:"currentSpeaker"`
	// AwaitingResponse is true from the moment the app layer claims
	// CurrentSpeaker for a turn prompt (TryClaimCurrentSpeaker) until
	// that participant (or any participant — see RecordPost) posts, or
	// the dispatch attempt fails (ClearAwaitingResponse). It's what
	// keeps a human's mid-flight interjection (PostChannelMessage) from
	// double-prompting a speaker who is already mid-turn.
	AwaitingResponse    bool              `json:"awaitingResponse"`
	TurnCount           int               `json:"turnCount"`
	MaxTurns            int               `json:"maxTurns"`
	ConclusionProposals map[string]string `json:"conclusionProposals"`
	Concluded           bool              `json:"concluded"`
}

// Deliberation manages turn alternation and conclusion proposals.
type Deliberation struct {
	state        DeliberationState
	participants []string
	mu           sync.Mutex
}

// NewDeliberation constructs a new deliberation state machine seeded
// with the participant roster in round-robin order. The roster must be
// known up front so the FSM can name the first speaker before anyone
// has posted — RecordPost no longer discovers participants lazily.
// CurrentSpeaker starts at participants[0], or "" when the roster is
// empty (defensive; production discussions always have >= 2
// participants per registry validation).
func NewDeliberation(channelID string, maxTurns int, participants []string) *Deliberation {
	if maxTurns <= 0 {
		maxTurns = DefaultMaxTurns
	}
	roster := append([]string(nil), participants...)
	var first string
	if len(roster) > 0 {
		first = roster[0]
	}
	return &Deliberation{
		state: DeliberationState{
			ChannelID:           channelID,
			CurrentSpeaker:      first,
			MaxTurns:            maxTurns,
			ConclusionProposals: make(map[string]string),
		},
		participants: roster,
	}
}

// RestoreDeliberation reconstructs a Deliberation from persisted SQLite
// state after an app restart. The in-memory FSM a prior process held is
// gone once the process exits, but the channel row (MaxTurns), the
// child-thread roster, and the channel_messages history give the app
// layer enough to compute an equivalent turnCount / currentSpeaker pair
// and rebuild a state machine that picks up where the old one left off.
//
// AwaitingResponse always comes back false: provider sessions do not
// survive a restart, so there is no live turn any prompt could be
// "awaiting" — the next human post is what re-arms the next prompt.
// If turnCount has already reached maxTurns the rebuilt deliberation
// comes back concluded with no current speaker, mirroring RecordPost's
// own conclusion semantics.
func RestoreDeliberation(channelID string, maxTurns int, participants []string, turnCount int, currentSpeaker string) *Deliberation {
	d := NewDeliberation(channelID, maxTurns, participants)
	d.state.TurnCount = turnCount
	d.state.CurrentSpeaker = currentSpeaker
	d.state.AwaitingResponse = false
	if turnCount >= d.state.MaxTurns {
		d.state.Concluded = true
		d.state.CurrentSpeaker = ""
	}
	return d
}

// TryClaimCurrentSpeaker atomically claims the current speaker for a
// turn-prompt dispatch: if a turn is already in flight
// (AwaitingResponse), the deliberation has concluded, or there is no
// current speaker (e.g. a single-participant roster), it returns
// ok=false and changes nothing.
//
// This is the only way AwaitingResponse flips to true, and it must
// happen synchronously in the same goroutine that decided to prompt —
// the app layer calls it BEFORE handing off to the background
// goroutine that actually dispatches the prompt (promptDiscussionSpeakerAsync).
// A two-step "read state, then decide to dispatch in a goroutine"
// sequence would leave a gap between the read and the eventual mark
// where a second trigger (e.g. a fast second human post landing while
// the first prompt's provider dispatch is still in flight) could also
// see AwaitingResponse=false and double-prompt the same speaker.
// Claiming atomically here closes that gap.
func (d *Deliberation) TryClaimCurrentSpeaker() (threadID string, ok bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.state.Concluded || d.state.AwaitingResponse || d.state.CurrentSpeaker == "" {
		return "", false
	}
	d.state.AwaitingResponse = true
	return d.state.CurrentSpeaker, true
}

// ClearAwaitingResponse un-claims a turn after a dispatch attempt
// failed to actually reach the provider (promptDiscussionSpeaker's send
// error path). Without this, a failed dispatch would leave
// AwaitingResponse stuck true forever: the participant never received
// the prompt, so nothing would ever post to clear it via RecordPost.
// Clearing it lets the next trigger (a human post, or another
// participant's turn landing) retry prompting the same CurrentSpeaker.
func (d *Deliberation) ClearAwaitingResponse() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.state.AwaitingResponse = false
}

// RecordPost records a participant post and returns the next speaker.
// A post from ANY participant counts as that participant's turn and
// advances round-robin from them — even one the FSM didn't just
// prompt, e.g. a human manually driving a child thread's pane directly
// instead of through the parent channel. AwaitingResponse always
// clears on a recorded post, whether or not the poster was the one the
// FSM was waiting on.
func (d *Deliberation) RecordPost(participantThreadID string) (nextSpeaker string, shouldConclude bool) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.state.Concluded {
		return "", true
	}

	d.rememberParticipant(participantThreadID)
	d.state.AwaitingResponse = false
	d.state.TurnCount++
	shouldConclude = d.state.TurnCount >= d.state.MaxTurns
	if shouldConclude {
		d.state.Concluded = true
		d.state.CurrentSpeaker = ""
		return "", true
	}

	nextSpeaker = d.nextSpeakerAfter(participantThreadID)
	d.state.CurrentSpeaker = nextSpeaker
	return nextSpeaker, false
}

// ProposeConclusionFrom tracks a participant conclusion proposal.
func (d *Deliberation) ProposeConclusionFrom(threadID, summary string) (allAgreed bool) {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.rememberParticipant(threadID)
	d.state.ConclusionProposals[threadID] = summary
	if len(d.participants) < 2 {
		return false
	}
	for _, participant := range d.participants {
		if _, ok := d.state.ConclusionProposals[participant]; !ok {
			return false
		}
	}
	d.state.Concluded = true
	d.state.CurrentSpeaker = ""
	return true
}

// State returns a defensive copy of the current deliberation state.
func (d *Deliberation) State() DeliberationState {
	d.mu.Lock()
	defer d.mu.Unlock()

	state := d.state
	state.ConclusionProposals = cloneProposals(d.state.ConclusionProposals)
	return state
}

// Participants returns a defensive copy of the roster in round-robin
// order. Used by the app layer to build the participants[] slice of
// the discussion:state event payload.
func (d *Deliberation) Participants() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.participants...)
}

func (d *Deliberation) rememberParticipant(threadID string) {
	if threadID == "" {
		return
	}
	for _, existing := range d.participants {
		if existing == threadID {
			return
		}
	}
	d.participants = append(d.participants, threadID)
}

func (d *Deliberation) nextSpeakerAfter(threadID string) string {
	return NextSpeakerAfter(d.participants, threadID)
}

// NextSpeakerAfter returns the participant that follows threadID in
// round-robin order over participants. Returns "" when there are fewer
// than two participants (a single-participant discussion must not
// self-loop). Falls back to the first participant when threadID isn't
// in the roster (mirrors rememberParticipant's lazy-add: a poster the
// FSM hasn't seen yet still gets a well-defined "next speaker").
//
// Exported so the app layer's restart-rebuild path (deliberationForChannel)
// can compute "the participant after the last agent poster" using the
// exact same round-robin rule the live FSM uses, rather than
// re-deriving it.
func NextSpeakerAfter(participants []string, threadID string) string {
	if len(participants) < 2 {
		return ""
	}
	for i, participant := range participants {
		if participant == threadID {
			return participants[(i+1)%len(participants)]
		}
	}
	return participants[0]
}

func cloneProposals(src map[string]string) map[string]string {
	dst := make(map[string]string, len(src))
	for key, value := range src {
		dst[key] = value
	}
	return dst
}

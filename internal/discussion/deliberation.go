package discussion

import "sync"

// DeliberationState tracks in-memory turn state for an active discussion.
type DeliberationState struct {
	ChannelID           string            `json:"channelId"`
	CurrentSpeaker      string            `json:"currentSpeaker"`
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

// NewDeliberation constructs a new deliberation state machine.
func NewDeliberation(channelID string, maxTurns int) *Deliberation {
	if maxTurns <= 0 {
		maxTurns = 8
	}
	return &Deliberation{
		state: DeliberationState{
			ChannelID:           channelID,
			MaxTurns:            maxTurns,
			ConclusionProposals: make(map[string]string),
		},
	}
}

// RecordPost records a participant post and returns the next speaker.
func (d *Deliberation) RecordPost(participantThreadID string) (nextSpeaker string, shouldConclude bool) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.state.Concluded {
		return "", true
	}

	d.rememberParticipant(participantThreadID)
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
	if len(d.participants) < 2 {
		return ""
	}
	for i, participant := range d.participants {
		if participant == threadID {
			return d.participants[(i+1)%len(d.participants)]
		}
	}
	return d.participants[0]
}

func cloneProposals(src map[string]string) map[string]string {
	dst := make(map[string]string, len(src))
	for key, value := range src {
		dst[key] = value
	}
	return dst
}

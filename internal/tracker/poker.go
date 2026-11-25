package tracker

import (
	"context"
	"errors"
	"math"
)

// Poker is a connection's planning poker: the votes a team casts on an
// issue's estimate, held by an add-on outside the tracker's own data
// (docs/references/tracker-api-measurements.md, "Planning poker add-on").
// Provider.Poker is nil for a tracker that cannot have one (Linear). A
// tracker that can is answered per site: Support says whether this site
// has the add-on, and every other call on a site without it fails
// wrapping ErrPokerAbsent (a read) or is refused with ReasonUnsupported
// (a write), before the add-on is called.
//
// A vote is the voter's alone: nothing here writes an issue's estimate.
// A person sets the agreed estimate as any other edit (SetEstimate).
type Poker interface {
	// Support reports whether the connection's site has the add-on.
	Support(ctx context.Context) (bool, error)
	// Read is issue's votes as the add-on holds them now.
	Read(ctx context.Context, issue IssueID) (PokerRound, error)
	// Vote casts or changes the caller's vote on in.Issue, after
	// in.Check; one StepPoker part.
	Vote(ctx context.Context, in PokerVoteInput) Result
	// Reveal shows every vote on issue; one StepPoker part.
	Reveal(ctx context.Context, issue IssueID) Result
	// Clear removes every vote on issue; one StepPoker part.
	Clear(ctx context.Context, issue IssueID) Result
}

// ErrPokerAbsent is matched by a poker read on a site whose tracker could
// have the add-on and does not.
var ErrPokerAbsent = errors.New("tracker: planning poker (Easy Agile TeamRhythm) is not installed on this site")

// PokerRound is an issue's votes. Before Revealed, a vote's Value is set
// only on the caller's own vote (Mine), whatever the add-on answered:
// the provider hides the rest. Catalog names the voters.
type PokerRound struct {
	Revealed bool
	Votes    []PokerVote
	Catalog
}

// PokerVote is one person's vote; Value is nil while it is hidden.
type PokerVote struct {
	Voter UserID
	Value *float64
	Mine  bool
}

// The vote range the add-on's vendor documents.
const (
	MinPokerVote = 0
	MaxPokerVote = 1000
)

// PokerVoteInput is a vote on Issue.
type PokerVoteInput struct {
	Issue IssueID
	Value float64
}

// Check refuses a vote with no issue, or a value that is not a number from
// MinPokerVote to MaxPokerVote.
func (in PokerVoteInput) Check() error {
	if err := CheckIssueID(in.Issue); err != nil {
		return err
	}
	if math.IsNaN(in.Value) || in.Value < MinPokerVote || in.Value > MaxPokerVote {
		return invalidf("a vote is a number from %d to %d", MinPokerVote, MaxPokerVote)
	}
	return nil
}

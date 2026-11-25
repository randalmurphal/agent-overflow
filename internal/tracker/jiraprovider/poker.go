package jiraprovider

import (
	"context"
	"errors"
	"fmt"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/jira"
)

// jiraPoker is the planning poker add-on of a Jira site, over the jira
// client's poker calls (jira/poker.go).
type jiraPoker struct{ jira *jira.Client }

var _ tracker.Poker = jiraPoker{}

// Poker is the site's planning poker add-on; whether the site has it is
// Support's answer.
func (p *Provider) Poker() tracker.Poker { return jiraPoker{jira: p.jira} }

// PokerAbsent is the sentence of poker on a site without the add-on: a
// write's refusal here, and a read's failure in trackerapp.
const PokerAbsent = "planning poker (Easy Agile TeamRhythm) is not installed on this Jira site"

// Support reports whether the site has the add-on.
func (j jiraPoker) Support(ctx context.Context) (bool, error) {
	ok, err := j.jira.PokerInstalled(ctx)
	if err != nil {
		return false, fmt.Errorf("jiraprovider: planning poker: %w", err)
	}
	return ok, nil
}

// Read is issue's votes. Before they are revealed, only the caller's own
// vote keeps its value, whatever the add-on answered (the measured test
// had one voter, so whether the add-on hides the others is not known).
// Voters are named from the client's users, one read for each the client
// has not kept (siteAge). A voter Jira withholds (missing, or a
// credential that may not see users) stays unnamed, shown by account ID,
// and the round stands; the client keeps that answer as long as a name.
func (j jiraPoker) Read(ctx context.Context, issue tracker.IssueID) (tracker.PokerRound, error) {
	round, err := j.jira.PokerVotes(ctx, string(issue))
	if errors.Is(err, jira.ErrPokerAbsent) {
		return tracker.PokerRound{}, fmt.Errorf("jiraprovider: planning poker of %s: %w", issue, errors.Join(tracker.ErrPokerAbsent, err))
	}
	if err != nil {
		return tracker.PokerRound{}, fmt.Errorf("jiraprovider: planning poker of %s: %w", issue, err)
	}
	me, err := j.jira.Me(ctx)
	if err != nil {
		return tracker.PokerRound{}, fmt.Errorf("jiraprovider: planning poker of %s: who the caller is: %w", issue, err)
	}
	out := tracker.PokerRound{Revealed: round.Revealed, Votes: make([]tracker.PokerVote, 0, len(round.Votes))}
	for _, v := range round.Votes {
		vote := tracker.PokerVote{Voter: tracker.UserID(v.Account), Mine: v.Account == me.AccountID}
		if round.Revealed || vote.Mine {
			value := v.Vote
			vote.Value = &value
		}
		out.Votes = append(out.Votes, vote)
		u, err := j.jira.PokerVoter(ctx, v.Account)
		switch {
		case errors.Is(err, jira.ErrUserWithheld):
		case err != nil:
			return tracker.PokerRound{}, fmt.Errorf("jiraprovider: planning poker of %s: voter %s: %w", issue, v.Account, err)
		default:
			out.Users = append(out.Users, userOf(u))
		}
	}
	return out, nil
}

// Vote casts or changes the caller's vote.
func (j jiraPoker) Vote(ctx context.Context, in tracker.PokerVoteInput) tracker.Result {
	if err := in.Check(); err != nil {
		return tracker.Invalid(tracker.StepPoker, err, in.Issue)
	}
	return pokerResult(in.Issue, j.jira.CastPokerVote(ctx, string(in.Issue), in.Value))
}

// Reveal shows every vote on issue.
func (j jiraPoker) Reveal(ctx context.Context, issue tracker.IssueID) tracker.Result {
	if err := tracker.CheckIssueID(issue); err != nil {
		return tracker.Invalid(tracker.StepPoker, err, issue)
	}
	return pokerResult(issue, j.jira.RevealPokerVotes(ctx, string(issue)))
}

// Clear removes every vote on issue.
func (j jiraPoker) Clear(ctx context.Context, issue tracker.IssueID) tracker.Result {
	if err := tracker.CheckIssueID(issue); err != nil {
		return tracker.Invalid(tracker.StepPoker, err, issue)
	}
	return pokerResult(issue, j.jira.ClearPokerVotes(ctx, string(issue)))
}

// pokerResult is the one StepPoker part of a poker write on issue whose
// client call returned err. It changes nothing the mirror holds.
func pokerResult(issue tracker.IssueID, err error) tracker.Result {
	if errors.Is(err, jira.ErrPokerAbsent) {
		return tracker.Refuse(tracker.StepPoker, tracker.Refusal{Reason: tracker.ReasonUnsupported, Message: PokerAbsent, Err: err}, issue)
	}
	return tracker.Result{Parts: []tracker.Part{{Issue: issue, Step: tracker.StepPoker, Outcome: outcome(err)}}}
}

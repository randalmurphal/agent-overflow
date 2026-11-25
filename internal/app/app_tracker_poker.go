package app

import (
	"context"
	"errors"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/tracker"
	"agent-overflow/internal/trackerapp"
)

// A ticket's planning poker (trackerapp/poker.go): whether a connection's
// site has it, the votes of a subscribed ticket while its poker panel is
// open, and the writes. A method that names a connection runs on the
// computer that holds it (family "trackerConnection"); one that names a
// subscription, on the computer that answered the subscribe (family
// "subscription"). No vote writes an issue's estimate.

// TrackerPokerSupport reports whether a connection's site has planning
// poker, so a ticket shows its control: a Linear or Tempo connection
// answers false at once; a Jira site's listing is read at most once in
// 30 minutes. A listing that fails answers false with its failure.
//
//ao:scope tracker:read
//ao:route home
func (a *App) TrackerPokerSupport(ctx context.Context, connectionID string) (trackerapp.PokerSupport, error) {
	return trackerFormRead(a, ctx, "TrackerPokerSupport", "Asking whether the site has planning poker", func(ctx context.Context, svc *trackerapp.Service) (trackerapp.PokerSupport, error) {
		return svc.PokerSupport(ctx, connectionID)
	})
}

// FollowTrackerPoker makes a ticket subscription follow its issue's votes
// while the client shows its poker panel, or stop, and answers the votes
// as last told. While followed and shown, the votes are read every 5
// seconds and tracker:issue_updated with poker set tells a change; a
// hidden ticket reads none. A Linear ticket is refused.
//
//ao:scope tracker:read
//ao:route home
func (a *App) FollowTrackerPoker(ctx context.Context, subscriptionID string, follow bool) (trackerapp.PokerView, error) {
	svc, err := a.trackerService()
	if err != nil {
		return trackerapp.PokerView{}, err
	}
	out, err := svc.FollowPoker(subscriptionID, follow)
	return out, pokerError(err)
}

// TrackerPoker reads a followed ticket's votes as its followers were last
// told, from memory: after an event with poker set.
//
//ao:scope tracker:read
//ao:route home
func (a *App) TrackerPoker(ctx context.Context, subscriptionID string) (trackerapp.PokerView, error) {
	svc, err := a.trackerService()
	if err != nil {
		return trackerapp.PokerView{}, err
	}
	out, err := svc.Poker(subscriptionID)
	return out, pokerError(err)
}

// VoteTrackerPoker casts or changes the caller's vote on an issue.
//
//ao:scope tracker:operate
//ao:route home
//ao:budget 3m
func (a *App) VoteTrackerPoker(ctx context.Context, connectionID string, in trackerapp.PokerVoteRequest) (trackerapp.WriteView, error) {
	return trackerWrite(a, ctx, "VoteTrackerPoker", tracker.StepPoker, connectionID, in, (*trackerapp.Service).PokerVote)
}

// RevealTrackerPoker shows every vote on an issue.
//
//ao:scope tracker:operate
//ao:route home
//ao:budget 3m
func (a *App) RevealTrackerPoker(ctx context.Context, connectionID, issue string) (trackerapp.WriteView, error) {
	return trackerWrite(a, ctx, "RevealTrackerPoker", tracker.StepPoker, connectionID, issue, (*trackerapp.Service).RevealPoker)
}

// ClearTrackerPokerVotes removes every vote on an issue.
//
//ao:scope tracker:operate
//ao:route home
//ao:budget 3m
func (a *App) ClearTrackerPokerVotes(ctx context.Context, connectionID, issue string) (trackerapp.WriteView, error) {
	return trackerWrite(a, ctx, "ClearTrackerPokerVotes", tracker.StepPoker, connectionID, issue, (*trackerapp.Service).ClearPoker)
}

// pokerError is a follow's or a read's error with its sentence: a tracker
// with no planning poker says so.
func pokerError(err error) error {
	if errors.Is(err, tracker.ErrUnsupported) {
		return errorsx.Public("tracker_unsupported", "this tracker has no planning poker", err)
	}
	return trackerError(err)
}

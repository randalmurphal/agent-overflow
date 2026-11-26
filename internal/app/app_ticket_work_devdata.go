package app

import (
	"context"
	"errors"
	"log"
	"slices"
	"strconv"
	"strings"
	"time"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/tracker"
	"agent-overflow/internal/trackerapp"
)

// ticketDevelopmentWait bounds how long Work's search waits for the
// tracker's development data: as long as an interactive tracker call
// waits for a closed rate gate (trackerapi's interactiveGateWait), so an
// endpoint that fails or stalls holds the forge's rows back no longer.
// Within it the transport still retries a 503 or a 429 it may.
const ticketDevelopmentWait = 10 * time.Second

// ticketDevelopment is a ticket's pull requests as the tracker's forge
// integrations link them (trackerapp.Service.IssuePullRequests, Jira's
// development data), read beside the forge search: the rows read, each
// read that failed, the hold's error, and the wait when it passed first.
type ticketDevelopment struct {
	prs      []tracker.LinkedPullRequest
	failures []trackerapp.DevelopmentFailure
	err      error
	timedOut time.Duration
}

// readTicketDevelopment reads what the tracker's forge integrations link
// to issueID, for at most ticketDevelopmentWait (the tracker budget in
// tests): on Jira one summary request, then one detail request per
// integration that links a pull request; on Linear none.
func (a *App) readTicketDevelopment(ctx context.Context, connectionID, issueID string) ticketDevelopment {
	wait := ticketDevelopmentWait
	if a.trackerBudget > 0 {
		wait = a.trackerBudget
	}
	waitCtx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	svc, err := a.trackerService()
	if err != nil {
		return ticketDevelopment{err: err}
	}
	prs, failures, err := svc.IssuePullRequests(waitCtx, connectionID, issueID)
	dev := ticketDevelopment{prs: prs, failures: failures, err: err}
	if errors.Is(waitCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
		dev.timedOut = wait
	}
	return dev
}

// addDevelopment lists the pull requests the tracker links to the ticket
// that no source read before it holds. They come after the stored list
// and the forge's search, which read the forge itself, so a pull request
// either listed stays as it is; one only a linked thread recorded, with no
// title or author, takes the tracker's row. The tracker's link is the
// evidence that a pull request is the ticket's, so its rows are not held
// to the key rule (namesTicketKey). A link whose URL names no pull
// request the app can address is left out and counted. A failure is
// said in DevelopmentProblem beside the other rows.
func (r *ticketWorkRead) addDevelopment(dev ticketDevelopment) {
	r.work.DevelopmentProblem = developmentProblem(dev)
	for _, linked := range dev.prs {
		ref, err := gitops.ParsePRURL(linked.URL)
		if err != nil {
			r.work.DevelopmentUnlisted++
			continue
		}
		row := TicketPullRequest{Ref: ref, URL: linked.URL, Title: linked.Title, Author: linked.Author, State: developmentState(linked), Branch: linked.Branch}
		id := ref.Identity()
		at := slices.IndexFunc(r.work.PullRequests, func(p TicketPullRequest) bool { return p.Ref.Identity() == id })
		switch {
		case at < 0:
			r.work.PullRequests = append(r.work.PullRequests, row)
		case r.recorded[id]:
			if row.Branch == "" {
				row.Branch = r.work.PullRequests[at].Branch
			}
			r.work.PullRequests[at] = row
			delete(r.recorded, id)
		}
	}
}

// developmentState is a linked pull request's state as Work names it: a
// status the tracker's word was not measured for shows as that word.
func developmentState(pr tracker.LinkedPullRequest) string {
	switch pr.State {
	case tracker.PullRequestOpen:
		return TicketPROpen
	case tracker.PullRequestMerged:
		return TicketPRMerged
	case tracker.PullRequestClosed:
		return TicketPRClosed
	}
	return pr.Status
}

// developmentProblem is what Work says of a development read that did
// not answer whole, empty when it did: that it did not answer within its
// wait, the hold's failure, or each failed read with where it was and
// the status its message does not say.
func developmentProblem(dev ticketDevelopment) string {
	const failed = "Jira's development data could not be read"
	switch {
	case dev.timedOut > 0:
		return "Jira's development data did not answer within " + waitText(dev.timedOut) + "; Refresh reads it again"
	case dev.err != nil:
		_, message := trackerRefusal(dev.err)
		if message == "" {
			log.Printf("ticket work: development data: %v", dev.err)
			message = "the app's log has the cause"
		}
		return failed + ": " + message
	case len(dev.failures) == 0:
		return ""
	}
	text := failed
	for i, f := range dev.failures {
		where := f.Where
		if f.Status != 0 {
			where = strings.TrimPrefix(where+", HTTP "+strconv.Itoa(f.Status), ", ")
		}
		switch {
		case i > 0:
			text += "; "
		case where == "":
			text += ": "
		default:
			text += " "
		}
		if where != "" {
			text += "(" + where + "): "
		}
		text += f.Failure.Message
	}
	return text
}

// waitText is a wait as a person reads it: whole seconds in words, a
// shorter wait (a test's) as Go writes it.
func waitText(d time.Duration) string {
	if d%time.Second == 0 {
		return strconv.Itoa(int(d/time.Second)) + " seconds"
	}
	return d.String()
}

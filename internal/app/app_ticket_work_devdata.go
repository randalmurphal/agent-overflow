package app

import (
	"context"
	"errors"
	"log"
	"net/url"
	"slices"
	"strings"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/trackerapi"
)

// ticketDevelopment is a ticket's pull requests as the tracker's forge
// integrations link them (trackerapp.Service.IssuePullRequests, Jira's
// development data), read beside the forge search.
type ticketDevelopment struct {
	prs []tracker.LinkedPullRequest
	err error
}

// readTicketDevelopment reads what the tracker's forge integrations link
// to issueID: on Jira one summary request, then one detail request per
// integration that links a pull request; on Linear none.
func (a *App) readTicketDevelopment(ctx context.Context, connectionID, issueID string) ticketDevelopment {
	svc, err := a.trackerService()
	if err != nil {
		return ticketDevelopment{err: err}
	}
	prs, err := svc.IssuePullRequests(ctx, connectionID, issueID)
	return ticketDevelopment{prs: prs, err: err}
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
	if dev.err != nil {
		r.work.DevelopmentProblem = developmentProblem(dev.err)
		return
	}
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

// developmentProblem is why the tracker's development data could not be
// read, as Work says it: the tracker's sentence for the failure, with the
// endpoint that answered when one did.
func developmentProblem(err error) string {
	where := ""
	if status, ok := errors.AsType[*trackerapi.StatusError](err); ok {
		if u, perr := url.Parse(status.URL); perr == nil && u.Path != "" {
			where = " (" + strings.TrimPrefix(u.Path, "/") + ")"
		}
	}
	_, message := trackerRefusal(err)
	if message == "" {
		log.Printf("ticket work: development data: %v", err)
		message = "the app's log has the cause"
	}
	return "Jira's development data could not be read" + where + ": " + message
}

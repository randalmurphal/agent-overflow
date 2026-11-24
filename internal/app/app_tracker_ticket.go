package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/tracker"
	"agent-overflow/internal/trackerapp"
	"agent-overflow/internal/transport"
)

// A ticket's bound methods: its subscription and read, the reads a write
// form needs, and the tickets a file crosses on (attachments, saved
// through the tracker routes in internal/transport/trackerroutes.go).
// A method that names a connection runs on the computer that holds it,
// which a client pins (methodFamilies.ts, family "trackerConnection"); one
// that names a subscription, on the computer that answered the subscribe.

// SubscribeTrackerTicket subscribes the calling client to an issue's
// ticket. The answer comes from this computer's copy of the issue; the
// ticket's reads of the tracker follow it, and tracker:issue_updated
// tells what they change. The subscription is released by
// ReleaseTrackerSubscription or when the client's connection ends, and
// takes SetTrackerSubscriptionVisible and RefreshTrackerSubscription as a
// board's does.
//
//ao:scope tracker:read
//ao:route home
//ao:callerbound
func (a *App) SubscribeTrackerTicket(ctx context.Context, connectionID, issue string) (trackerapp.TicketSubscription, error) {
	conn, err := transport.CallerConnection(ctx)
	if err != nil {
		return trackerapp.TicketSubscription{}, err
	}
	svc, err := a.trackerService()
	if err != nil {
		return trackerapp.TicketSubscription{}, err
	}
	sub, err := svc.SubscribeTicket(connectionID, tracker.IssueID(issue), trackerSubscriptionTie(conn, svc))
	return sub, trackerError(err)
}

// TrackerTicket reads a subscribed ticket as its subscribers were last
// told of it, from memory: after an event that says it changed, a
// reconnect, or a gap.
//
//ao:scope tracker:read
//ao:route home
func (a *App) TrackerTicket(ctx context.Context, subscriptionID string) (trackerapp.TicketView, error) {
	svc, err := a.trackerService()
	if err != nil {
		return trackerapp.TicketView{}, err
	}
	out, err := svc.TicketDetail(subscriptionID)
	return out, trackerError(err)
}

// MintTrackerAttachmentUploadTicket returns the URL a client PUTs one
// file of size bytes to, attaching it to the issue as filename; the PUT
// answers the write's outcome. A tracker that stores no files (Linear),
// and a file larger than its tracker stores, are refused here, before any
// byte crosses the network.
//
//ao:scope tracker:operate
//ao:route home
func (a *App) MintTrackerAttachmentUploadTicket(ctx context.Context, connectionID, issue, filename string, size int64) (string, error) {
	switch {
	case issue == "" || filename == "" || size < 0:
		return "", errorsx.Public("tracker_request_invalid", "nothing was sent: an attachment needs a ticket, a file name and a size", trackerapp.ErrInvalidRequest)
	case strings.ContainsRune(filename, 0):
		return "", errorsx.Public("tracker_request_invalid", "nothing was sent: the file name holds a NUL character", trackerapp.ErrInvalidRequest)
	}
	svc, err := a.trackerService()
	if err != nil {
		return "", err
	}
	ceiling, err := svc.AttachmentCeiling(ctx, connectionID)
	switch {
	case err != nil:
		return "", trackerError(err)
	case ceiling == 0:
		return "", errorsx.Public("tracker_attachments_unsupported", "this tracker does not store files, so nothing can be attached to its tickets", tracker.ErrUnsupported)
	case size > ceiling:
		return "", errorsx.Public("tracker_attachment_too_large",
			fmt.Sprintf("the tracker stores files up to %s, and this one is %s; share it another way and link to it", byteSize(ceiling), byteSize(size)),
			fmt.Errorf("%w: a file of %d bytes, over %d", tracker.ErrInvalidInput, size, ceiling))
	}
	server := a.transportServer.Load()
	if server == nil {
		return "", errors.New("tracker attachment: transport is not serving")
	}
	return server.MintTrackerUploadTicket(connectionID, issue, filename, size)
}

// MintTrackerAttachmentDownloadTicket returns the URL a client GETs one
// attachment of a subscribed ticket from, saved under its name. It asks
// the tracker nothing: the attachment must be on the ticket as its
// subscribers were last told of it.
//
//ao:scope tracker:read
//ao:route home
func (a *App) MintTrackerAttachmentDownloadTicket(ctx context.Context, subscriptionID, attachmentID string) (string, error) {
	svc, err := a.trackerService()
	if err != nil {
		return "", err
	}
	connection, issue, att, err := svc.TicketAttachment(subscriptionID, attachmentID)
	if err != nil {
		return "", trackerError(err)
	}
	server := a.transportServer.Load()
	if server == nil {
		return "", errors.New("tracker attachment: transport is not serving")
	}
	return server.MintTrackerDownloadTicket(connection, string(issue), att.ID, att.Name)
}

// byteSize is n bytes as a person reads a file's size.
func byteSize(n int64) string {
	switch {
	case n >= 1<<30 && n%(1<<30) == 0:
		return fmt.Sprintf("%d GB", n>>30)
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d bytes", n)
}

// TrackerTransfer adapts the tracker service onto the transport's tracker
// routes. A package function, not a method, so it never becomes an RPC.
func TrackerTransfer(a *App) transport.TrackerTransfer { return trackerTransfer{app: a} }

type trackerTransfer struct{ app *App }

// AttachTrackerFile attaches the upload's file. The write runs on this
// computer's lifetime with the request's values, so a client that leaves
// after its last byte still has its file attached and its ticket told; one
// that leaves before stops the body, and the write is not applied.
func (t trackerTransfer) AttachTrackerFile(upload transport.TrackerUpload) (json.RawMessage, error) {
	svc, err := t.app.trackerService()
	if err != nil {
		return nil, err
	}
	view, err := svc.Attach(context.WithoutCancel(upload.Context), upload.Connection, upload.Issue,
		[]tracker.File{{Name: upload.Filename, Content: upload.Body, Size: upload.Size}})
	if err != nil {
		return nil, trackerError(err)
	}
	out, err := json.Marshal(view)
	if err != nil {
		return nil, fmt.Errorf("tracker attachment: encode the write's outcome: %w", err)
	}
	return out, nil
}

// DownloadTrackerAttachment streams one attachment to to, ending with ctx.
func (t trackerTransfer) DownloadTrackerAttachment(ctx context.Context, connection, issue, attachment string, to io.Writer) error {
	svc, err := t.app.trackerService()
	if err != nil {
		return err
	}
	return trackerError(svc.Download(ctx, connection, issue, attachment, to))
}

// The reads a write form needs. Each asks the tracker and is bounded, the
// whole call, by its method's budget (trackerRun), so the answer
// arrives before the client stops waiting and says the tracker was too
// slow and to try again. Their context reaches the network, so none is
// caller-bound.

// TrackerIssueTypes lists the issue types a project's create form offers.
//
//ao:scope tracker:read
//ao:route home
func (a *App) TrackerIssueTypes(ctx context.Context, connectionID, project string) ([]trackerapp.IssueTypeView, error) {
	return trackerFormRead(a, ctx, "TrackerIssueTypes", "Listing the project's issue types", func(ctx context.Context, svc *trackerapp.Service) ([]trackerapp.IssueTypeView, error) {
		return svc.IssueTypes(ctx, connectionID, project)
	})
}

// TrackerCreateFields lists the fields a create of issueType in project
// takes.
//
//ao:scope tracker:read
//ao:route home
func (a *App) TrackerCreateFields(ctx context.Context, connectionID, project, issueType string) ([]trackerapp.FieldView, error) {
	return trackerFormRead(a, ctx, "TrackerCreateFields", "Reading the create form's fields", func(ctx context.Context, svc *trackerapp.Service) ([]trackerapp.FieldView, error) {
		return svc.CreateFields(ctx, connectionID, project, issueType)
	})
}

// TrackerMoves lists the moves an issue's workflow allows now, with the
// fields each one's screen takes.
//
//ao:scope tracker:read
//ao:route home
func (a *App) TrackerMoves(ctx context.Context, connectionID, issue string) ([]trackerapp.MoveView, error) {
	return trackerFormRead(a, ctx, "TrackerMoves", "Reading the ticket's moves", func(ctx context.Context, svc *trackerapp.Service) ([]trackerapp.MoveView, error) {
		return svc.Moves(ctx, connectionID, issue)
	})
}

// TrackerAssignableUsers lists the users text matches who may be assigned
// in scope.
//
//ao:scope tracker:read
//ao:route home
func (a *App) TrackerAssignableUsers(ctx context.Context, connectionID string, scope trackerapp.FieldScopeInput, text string) (trackerapp.UserListView, error) {
	return trackerFormRead(a, ctx, "TrackerAssignableUsers", "Finding the people who may be assigned", func(ctx context.Context, svc *trackerapp.Service) (trackerapp.UserListView, error) {
		return svc.AssignableUsers(ctx, connectionID, scope, text)
	})
}

// TrackerChoices lists the values text matches that field allows in
// scope.
//
//ao:scope tracker:read
//ao:route home
func (a *App) TrackerChoices(ctx context.Context, connectionID string, scope trackerapp.FieldScopeInput, field, text string) (trackerapp.ChoiceListView, error) {
	return trackerFormRead(a, ctx, "TrackerChoices", "Listing the field's values", func(ctx context.Context, svc *trackerapp.Service) (trackerapp.ChoiceListView, error) {
		return svc.Choices(ctx, connectionID, scope, field, text)
	})
}

// TrackerLinkTypes lists the tracker's link types.
//
//ao:scope tracker:read
//ao:route home
func (a *App) TrackerLinkTypes(ctx context.Context, connectionID string) ([]trackerapp.LinkTypeView, error) {
	return trackerFormRead(a, ctx, "TrackerLinkTypes", "Listing the link types", func(ctx context.Context, svc *trackerapp.Service) ([]trackerapp.LinkTypeView, error) {
		return svc.LinkTypes(ctx, connectionID)
	})
}

// TrackerLookup finds the issues keys name.
//
//ao:scope tracker:read
//ao:route home
func (a *App) TrackerLookup(ctx context.Context, connectionID string, keys []string) (trackerapp.LookupView, error) {
	return trackerFormRead(a, ctx, "TrackerLookup", "Looking up the keys", func(ctx context.Context, svc *trackerapp.Service) (trackerapp.LookupView, error) {
		return svc.Lookup(ctx, connectionID, keys)
	})
}

// TrackerSearch is one page of the issues a search's text matches.
//
//ao:scope tracker:read
//ao:route home
func (a *App) TrackerSearch(ctx context.Context, connectionID string, in trackerapp.SearchRequest) (trackerapp.SearchPageView, error) {
	return trackerFormRead(a, ctx, "TrackerSearch", "Searching the tracker", func(ctx context.Context, svc *trackerapp.Service) (trackerapp.SearchPageView, error) {
		return svc.Search(ctx, connectionID, in)
	})
}

// TrackerComments reads a page of an issue's comments after the first,
// which the ticket holds.
//
//ao:scope tracker:read
//ao:route home
func (a *App) TrackerComments(ctx context.Context, connectionID, issue, page string) (trackerapp.CommentPageView, error) {
	return trackerFormRead(a, ctx, "TrackerComments", "Reading earlier comments", func(ctx context.Context, svc *trackerapp.Service) (trackerapp.CommentPageView, error) {
		return svc.Comments(ctx, connectionID, issue, page)
	})
}

// trackerFormRead runs read, bounded by method's budget, and answers its
// refusal's sentence.
func trackerFormRead[T any](a *App, ctx context.Context, method, doing string, read func(context.Context, *trackerapp.Service) (T, error)) (T, error) {
	svc, err := a.trackerService()
	if err != nil {
		var zero T
		return zero, err
	}
	ctx, cancel := a.trackerRun(ctx, method, doing, trackerSlow)
	defer cancel()
	out, err := read(ctx, svc)
	return out, trackerRunError(ctx, err)
}

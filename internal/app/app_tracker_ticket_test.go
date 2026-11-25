package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/harness/trackerfake"
	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/trackerapi"
	"agent-overflow/internal/trackerapp"
	"agent-overflow/internal/transport"
)

func TestTrackerTicketMethodsCarryTheirScopeAndRoute(t *testing.T) {
	t.Parallel()
	type want struct {
		scope       transport.Scope
		route       transport.MethodRoute
		callerBound bool
		budget      time.Duration
	}
	read, operate := transport.ScopeTrackerRead, transport.ScopeTrackerOperate
	// A method that names a connection is routed by its client (family
	// trackerConnection) and generated as home.
	write := want{operate, transport.RouteHome, false, trackerWriteBudget}
	form := want{read, transport.RouteHome, false, 0}
	for name, w := range map[string]want{
		"SubscribeTrackerTicket":              {read, transport.RouteHome, true, 0},
		"TrackerTicket":                       {read, transport.RouteHome, false, 0},
		"MintTrackerAttachmentUploadTicket":   {operate, transport.RouteHome, false, 0},
		"MintTrackerAttachmentDownloadTicket": {read, transport.RouteHome, false, 0},
		"SaveTrackerAttachment":               {operate, transport.RouteHome, false, 30 * time.Minute},
		"TrackerIssueTypes":                   form,
		"TrackerCreateFields":                 form,
		"TrackerMoves":                        form,
		"TrackerAssignableUsers":              form,
		"TrackerChoices":                      form,
		"TrackerLinkTypes":                    form,
		"TrackerLookup":                       form,
		"TrackerSearch":                       form,
		"TrackerComments":                     form,
		"CreateTrackerIssue":                  write,
		"EditTrackerIssue":                    write,
		"MoveTrackerIssue":                    write,
		"SetTrackerEstimate":                  write,
		"RankTrackerIssues":                   write,
		"MoveTrackerIssuesToSprint":           write,
		"CreateTrackerSprint":                 write,
		"StartTrackerSprint":                  write,
		"CompleteTrackerSprint":               write,
		"AddTrackerComment":                   write,
		"EditTrackerComment":                  write,
		"DeleteTrackerComment":                write,
		"LinkTrackerIssues":                   write,
		"UnlinkTrackerIssues":                 write,
		"AddTrackerWorklog":                   write,
		"EditTrackerSprint":                   write,
		"EditTrackerWorklog":                  write,
		"DeleteTrackerWorklog":                write,
		"TrackerWorklogs":                     form,
		"RefreshTrackerWorklogs":              form,
	} {
		i := slices.IndexFunc(transport.GeneratedMethods, func(m transport.MethodMeta) bool { return m.Name == name })
		if i < 0 {
			t.Errorf("%s is not a generated method", name)
			continue
		}
		m := transport.GeneratedMethods[i]
		if m.Scope != w.scope || m.Route != w.route || m.CallerBound != w.callerBound || m.Budget != w.budget {
			t.Errorf("%s = %+v, want scope %s, route %s, caller-bound %v, budget %s", name, m, w.scope, w.route, w.callerBound, w.budget)
		}
	}
}

// serveTrackerTransfers gives f's App a live transport whose tracker
// routes reach its tracker service, and answers its base URL.
func (f *trackerBoardFixture) serveTrackerTransfers(t *testing.T) string {
	t.Helper()
	srv, err := transport.New(transport.Config{
		Dispatcher:      transport.NewDispatcher(),
		EventBus:        transport.NewEventBus(8),
		Token:           "tracker-transfer-test-token",
		TrackerTransfer: TrackerTransfer(f.a),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	f.a.SetTransportServer(srv)
	return "http://" + srv.Addr()
}

// issueEvent waits for the next tracker:issue_updated event that ok
// accepts, skipping every other.
func (f *trackerBoardFixture) issueEvent(t *testing.T, what string, ok func(trackerapp.IssueEvent) bool) trackerapp.IssueEvent {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case e := <-f.events.Events():
			if e.Channel != string(eventchan.TrackerIssueUpdated) {
				continue
			}
			var got trackerapp.IssueEvent
			if err := json.Unmarshal(e.Data, &got); err != nil {
				t.Fatal(err)
			}
			if ok(got) {
				return got
			}
		case <-deadline:
			t.Fatalf("no tracker:issue_updated event %s", what)
		}
	}
}

// ticketShows waits until the subscribed ticket, read as a client reads it
// after an event, satisfies ok.
func (f *trackerBoardFixture) ticketShows(t *testing.T, ctx context.Context, sub, what string, ok func(*trackerapp.DetailView) bool) *trackerapp.DetailView {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		v, err := f.a.TrackerTicket(ctx, sub)
		if err != nil {
			t.Fatal(err)
		}
		if v.Loaded && ok(v.Detail) {
			return v.Detail
		}
		if time.Now().After(deadline) {
			t.Fatalf("the ticket never showed %s: %+v", what, v)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func appliedOnly(v trackerapp.WriteView) bool {
	return len(v.Parts) > 0 && !slices.ContainsFunc(v.Parts, func(p trackerapp.PartView) bool { return p.State != "applied" })
}

// A ticket is reached end to end from a client, through the bound methods
// and the byte routes with the real providers and transport against the
// fake Jira: the subscribe, the event of its first read, a comment written
// and shown with no refresh, a file attached and saved back byte for
// byte, and the release.
func TestATrackerTicketReachesItsClient(t *testing.T) {
	t.Parallel()
	f := newTrackerBoardFixture(t)
	base := f.serveTrackerTransfers(t)
	ctx, _ := newTestConnection(t, transport.ConnPrincipal{})

	sub, err := f.a.SubscribeTrackerTicket(ctx, f.conn.ID, "10101", true)
	if err != nil {
		t.Fatal(err)
	}
	f.issueEvent(t, "for the first read", func(e trackerapp.IssueEvent) bool {
		return e.Connection == f.conn.ID && e.Issue == "10101" && e.Changed && e.Seq > sub.Seq
	})
	f.ticketShows(t, ctx, sub.ID, "its title", func(d *trackerapp.DetailView) bool { return d.Card.Title == "First story" })

	v, err := f.a.AddTrackerComment(ctx, f.conn.ID, trackerapp.CommentRequest{Issue: "10101", Markdown: "Looks good"})
	if err != nil || !appliedOnly(v) || len(v.Stale) != 0 {
		t.Fatalf("AddTrackerComment = %+v, %v", v, err)
	}
	f.ticketShows(t, ctx, sub.ID, "the comment", func(d *trackerapp.DetailView) bool {
		return slices.ContainsFunc(d.Comments, func(c trackerapp.CommentView) bool { return strings.Contains(c.Body, "Looks good") })
	})

	payload := bytes.Repeat([]byte("attached bytes\n"), 10_000)
	att := f.attach(t, ctx, base, sub.ID, "notes.txt", payload)
	download, err := f.a.MintTrackerAttachmentDownloadTicket(ctx, sub.ID, att.ID)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get(base + download)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK || !bytes.Equal(got, payload) {
		t.Fatalf("the download answered %d, %d bytes, %v", resp.StatusCode, len(got), err)
	}
	if cd := resp.Header.Get("Content-Disposition"); cd != "attachment; filename=notes.txt" {
		t.Fatalf("Content-Disposition = %q", cd)
	}

	if err := f.a.ReleaseTrackerSubscription(ctx, sub.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.a.TrackerTicket(ctx, sub.ID); !hasCode(err, "tracker_subscription_unknown") {
		t.Fatalf("a released ticket = %v", err)
	}
	if _, err := f.a.MintTrackerAttachmentDownloadTicket(ctx, sub.ID, att.ID); !hasCode(err, "tracker_subscription_unknown") {
		t.Fatalf("a download from a released ticket = %v", err)
	}
}

// attach puts payload on issue 10101 as name through the upload route, as
// a client does, and answers the attachment once the subscribed ticket sub
// shows it.
func (f *trackerBoardFixture) attach(t *testing.T, ctx context.Context, base, sub, name string, payload []byte) trackerapp.AttachmentView {
	t.Helper()
	upload, err := f.a.MintTrackerAttachmentUploadTicket(ctx, f.conn.ID, "10101", name, int64(len(payload)))
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPut, base+upload, bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var attached trackerapp.WriteView
	err = json.NewDecoder(resp.Body).Decode(&attached)
	_ = resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK || !appliedOnly(attached) {
		t.Fatalf("the upload answered %d, %+v, %v", resp.StatusCode, attached, err)
	}
	named := func(a trackerapp.AttachmentView) bool { return a.Name == name }
	d := f.ticketShows(t, ctx, sub, "the attachment "+name, func(d *trackerapp.DetailView) bool {
		return slices.ContainsFunc(d.Attachments, named)
	})
	return d.Attachments[slices.IndexFunc(d.Attachments, named)]
}

// An attachment saved on the computer lands whole in its Downloads
// folder, a second save beside it rather than over it; a save the tracker
// fails leaves no file; an attachment the ticket does not show, or a
// released ticket, is refused before anything is asked.
func TestATrackerAttachmentSavesIntoDownloads(t *testing.T) {
	t.Parallel()
	f := newTrackerBoardFixture(t)
	base := f.serveTrackerTransfers(t)
	ctx, _ := newTestConnection(t, transport.ConnPrincipal{})
	sub, err := f.a.SubscribeTrackerTicket(ctx, f.conn.ID, "10101", true)
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("saved bytes\n"), 50_000)
	att := f.attach(t, ctx, base, sub.ID, "report.pdf", payload)
	downloads := filepath.Join(f.a.configDir, "downloads")

	first, err := f.a.SaveTrackerAttachment(ctx, sub.ID, att.ID)
	if err != nil || first != filepath.Join(downloads, "report.pdf") {
		t.Fatalf("SaveTrackerAttachment = %q, %v", first, err)
	}
	if got, err := os.ReadFile(first); err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("the saved file holds %d bytes, %v; want %d", len(got), err, len(payload))
	}
	second, err := f.a.SaveTrackerAttachment(ctx, sub.ID, att.ID)
	if err != nil || second != filepath.Join(downloads, "report (2).pdf") {
		t.Fatalf("a second save = %q, %v", second, err)
	}

	const route = "GET rest/api/3/attachment/content/{id}"
	if err := f.e.AddFault(trackerfake.Fault{Origin: trackerBoardOrigin, Route: route, Kind: trackerfake.FaultRefuse, Status: 403, Body: json.RawMessage(`{"errorMessages":["down"]}`)}); err != nil {
		t.Fatal(err)
	}
	if path, err := f.a.SaveTrackerAttachment(ctx, sub.ID, att.ID); err == nil {
		t.Fatalf("a save the tracker failed = %q", path)
	}
	f.e.ClearFaults(trackerBoardOrigin, route)
	if _, err := os.Stat(filepath.Join(downloads, "report (3).pdf")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a failed save left a file: %v", err)
	}

	if _, err := f.a.SaveTrackerAttachment(ctx, sub.ID, "999999"); !hasCode(err, "tracker_attachment_unknown") {
		t.Fatalf("an attachment the ticket does not show = %v", err)
	}
	if err := f.a.ReleaseTrackerSubscription(ctx, sub.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.a.SaveTrackerAttachment(ctx, sub.ID, att.ID); !hasCode(err, "tracker_subscription_unknown") {
		t.Fatalf("a save from a released ticket = %v", err)
	}
}

// A file whose client stops sending before its end is not attached, and
// the answer says so and what to do.
func TestATrackerUploadCutShortAttachesNothing(t *testing.T) {
	t.Parallel()
	f := newTrackerBoardFixture(t)
	base := f.serveTrackerTransfers(t)
	ctx, _ := newTestConnection(t, transport.ConnPrincipal{})
	upload, err := f.a.MintTrackerAttachmentUploadTicket(ctx, f.conn.ID, "10101", "notes.txt", 10)
	if err != nil {
		t.Fatal(err)
	}
	// Five bytes of the ten, sent without a declared length, then the end.
	req, err := http.NewRequest(http.MethodPut, base+upload, io.MultiReader(strings.NewReader("12345")))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var v trackerapp.WriteView
	err = json.NewDecoder(resp.Body).Decode(&v)
	_ = resp.Body.Close()
	if err != nil || len(v.Parts) != 1 || v.Parts[0].State != "not_sent" || v.Parts[0].Failure == nil ||
		v.Parts[0].Failure.Kind != string(trackerapp.FailureUpload) {
		t.Fatalf("a short upload answered %d, %+v, %v", resp.StatusCode, v, err)
	}
	ticket, err := f.a.SubscribeTrackerTicket(ctx, f.conn.ID, "10101", true)
	if err != nil {
		t.Fatal(err)
	}
	d := f.ticketShows(t, ctx, ticket.ID, "its first read", func(*trackerapp.DetailView) bool { return true })
	if len(d.Attachments) != 0 {
		t.Fatalf("a short upload attached %+v", d.Attachments)
	}
}

// Every hop gives a carried transfer's answer transport.AnswerAllowance to
// begin, and the attach is bounded by it: it covers what an attach does
// after its last byte (the tracker's answer and the refresh) and what a
// download waits for its first byte (the tracker's answer and its first
// body byte), each with a minute for the answer to cross the hops. A
// relay's wait starts when it reads the last byte, before the backend's
// does, so an allowance of exactly the backend's longest would cut an
// answer the backend gives at its bound.
func TestTheAnswerAllowanceCoversTheTracker(t *testing.T) {
	t.Parallel()
	const crossing = time.Minute
	if need := trackerapi.AnswerTimeout + trackerWriteBudget + crossing; transport.AnswerAllowance < need {
		t.Errorf("AnswerAllowance %s is shorter than an attach's answer and refresh, %s", transport.AnswerAllowance, need)
	}
	if need := trackerapi.AnswerTimeout + transport.TransferStallWindow + crossing; transport.AnswerAllowance < need {
		t.Errorf("AnswerAllowance %s is shorter than a download's wait for its first byte, %s", transport.AnswerAllowance, need)
	}
}

// stallUpload PUTs the first half of a size-byte file to the upload path
// over a raw connection that then sends nothing more, as a client whose
// network went away mid-upload does, and answers the connection to read
// the answer from.
func stallUpload(t *testing.T, base, path string, size int) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", strings.TrimPrefix(base, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	head := fmt.Sprintf("PUT %s HTTP/1.1\r\nHost: %s\r\nContent-Length: %d\r\n\r\n", path, strings.TrimPrefix(base, "http://"), size)
	if _, err := conn.Write(append([]byte(head), bytes.Repeat([]byte("x"), size/2)...)); err != nil {
		t.Fatal(err)
	}
	return conn
}

// answerOf reads the HTTP answer on conn within 15 seconds.
func answerOf(t *testing.T, conn net.Conn, what string) (*http.Response, []byte) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(15 * time.Second)); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("%s: no answer within 15s: %v", what, err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	return resp, body
}

// An upload whose client stops sending ends at the attach's bound, shortened
// here (trackerBudget): the read of the stalled body is ended, and the part
// is not sent, for the file's own failure.
func TestATrackerUploadWhoseClientStallsEndsAtItsBound(t *testing.T) {
	t.Parallel()
	f := newTrackerBoardFixtureOn(t, trackerBoardSetup{site: trackerBoardSite(), budget: time.Second})
	base := f.serveTrackerTransfers(t)
	ctx, _ := newTestConnection(t, transport.ConnPrincipal{})
	const size = 64 << 10
	path, err := f.a.MintTrackerAttachmentUploadTicket(ctx, f.conn.ID, "10101", "notes.txt", size)
	if err != nil {
		t.Fatal(err)
	}
	resp, body := answerOf(t, stallUpload(t, base, path, size), "a stalled upload")
	var v trackerapp.WriteView
	if err := json.Unmarshal(body, &v); err != nil || resp.StatusCode != http.StatusOK || len(v.Parts) != 1 ||
		v.Parts[0].State != "not_sent" || v.Parts[0].Failure == nil || v.Parts[0].Failure.Kind != string(trackerapp.FailureUpload) {
		t.Fatalf("a stalled upload answered %d %s (%v)", resp.StatusCode, body, err)
	}
}

// The tracker service stops promptly with an upload in flight whose client
// stopped sending, and the upload is answered.
func TestTrackersStopWithAStalledUploadInFlight(t *testing.T) {
	t.Parallel()
	arrived := make(chan struct{}, 1)
	f := newTrackerBoardFixtureOn(t, trackerBoardSetup{site: trackerBoardSite(), arrive: func(r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/attachments") {
			arrived <- struct{}{}
		}
	}})
	base := f.serveTrackerTransfers(t)
	ctx, _ := newTestConnection(t, transport.ConnPrincipal{})
	const size = 64 << 10
	path, err := f.a.MintTrackerAttachmentUploadTicket(ctx, f.conn.ID, "10101", "notes.txt", size)
	if err != nil {
		t.Fatal(err)
	}
	conn := stallUpload(t, base, path, size)
	select {
	case <-arrived:
	case <-time.After(15 * time.Second):
		t.Fatal("the upload never reached the tracker")
	}
	stopped := make(chan struct{})
	go func() {
		f.a.trackers.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(15 * time.Second):
		t.Fatal("the tracker service did not stop within 15s with a stalled upload in flight")
	}
	if resp, body := answerOf(t, conn, "an upload the stop ended"); resp.StatusCode == http.StatusOK {
		t.Fatalf("an upload the stop ended answered %d %s", resp.StatusCode, body)
	}
}

// A write the tracker holds past its method's budget answers within it,
// through the bound method, with the part told as unknown and what to do:
// look before trying again. The budget is shortened (trackerBudget).
func TestATrackerWriteHeldPastItsBudgetSaysToLook(t *testing.T) {
	t.Parallel()
	f := newTrackerBoardFixtureOn(t, trackerBoardSetup{site: trackerBoardSite(), budget: 2 * time.Second})
	const route = "POST rest/api/3/issue/{issue}/comment"
	if err := f.e.AddFault(trackerfake.Fault{Origin: trackerBoardOrigin, Route: route, Kind: trackerfake.FaultHold}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.e.ClearFaults(trackerBoardOrigin, route) })
	answer := make(chan error, 1)
	var v trackerapp.WriteView
	go func() {
		var err error
		v, err = f.a.AddTrackerComment(context.Background(), f.conn.ID, trackerapp.CommentRequest{Issue: "10101", Markdown: "Held"})
		answer <- err
	}()
	select {
	case err := <-answer:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("a held write did not end at its budget")
	}
	if len(v.Parts) != 1 || v.Parts[0].State != "unknown" || v.Parts[0].Failure == nil ||
		!strings.HasSuffix(v.Parts[0].Failure.Message, "look at the ticket before you try again") {
		t.Fatalf("a held comment answered %+v", v)
	}
}

// Each refusal a ticket's methods add carries its code, a sentence of
// what to do next, and the service's error as its cause.
func TestTrackerTicketRefusalsSayWhatToDo(t *testing.T) {
	t.Parallel()
	f := newTrackerBoardFixture(t)
	f.serveTrackerTransfers(t)
	ctx, _ := newTestConnection(t, transport.ConnPrincipal{})
	sub, err := f.a.SubscribeTrackerTicket(ctx, f.conn.ID, "10101", true)
	if err != nil {
		t.Fatal(err)
	}
	f.ticketShows(t, ctx, sub.ID, "its first read", func(*trackerapp.DetailView) bool { return true })
	linear, err := f.a.AddTrackerConnection(ctx, NewTrackerConnection{Kind: store.TrackerKindLinear, Site: "example"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(linear.KeyFilePath, []byte("lin_api_"+strings.Repeat("x", 40)), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, code string
		cause      error
		call       func() error
	}{
		{"a file over the tracker's largest", "tracker_attachment_too_large", tracker.ErrInvalidInput, func() error {
			return ignoreResult(f.a.MintTrackerAttachmentUploadTicket(ctx, f.conn.ID, "10101", "huge.bin", 3<<30))
		}},
		{"a file for a tracker that stores none", "tracker_attachments_unsupported", tracker.ErrUnsupported, func() error {
			return ignoreResult(f.a.MintTrackerAttachmentUploadTicket(ctx, linear.ID, "ENG-1", "a.txt", 1))
		}},
		{"a file name with a NUL", "tracker_request_invalid", tracker.ErrInvalidInput, func() error {
			return ignoreResult(f.a.MintTrackerAttachmentUploadTicket(ctx, f.conn.ID, "10101", "a\x00b", 1))
		}},
		{"a file name over 1,024 bytes", "tracker_request_invalid", tracker.ErrInvalidInput, func() error {
			return ignoreResult(f.a.MintTrackerAttachmentUploadTicket(ctx, f.conn.ID, "10101", strings.Repeat("n", 1025), 1))
		}},
		{"an upload to an issue ID over 64 bytes", "tracker_request_invalid", tracker.ErrInvalidInput, func() error {
			return ignoreResult(f.a.MintTrackerAttachmentUploadTicket(ctx, f.conn.ID, strings.Repeat("1", 65), "a.txt", 1))
		}},
		{"a ticket of an issue ID over 64 bytes", "tracker_request_invalid", tracker.ErrInvalidInput, func() error {
			return ignoreResult(f.a.SubscribeTrackerTicket(ctx, f.conn.ID, strings.Repeat("1", 65), true))
		}},
		{"an attachment the ticket does not show", "tracker_attachment_unknown", trackerapp.ErrUnknownAttachment, func() error {
			return ignoreResult(f.a.MintTrackerAttachmentDownloadTicket(ctx, sub.ID, "99999"))
		}},
		{"a request the conversion refuses", "tracker_request_invalid", trackerapp.ErrInvalidRequest, func() error {
			return ignoreResult(f.a.AddTrackerComment(ctx, f.conn.ID, trackerapp.CommentRequest{Issue: "10101", Markdown: "x", Audience: "everyone"}))
		}},
		{"a ticket of no issue", "tracker_request_invalid", trackerapp.ErrInvalidRequest, func() error {
			return ignoreResult(f.a.SubscribeTrackerTicket(ctx, f.conn.ID, "", true))
		}},
		{"a form read the tracker fails", "tracker_failed_tracker", nil, func() error {
			return ignoreResult(f.a.TrackerMoves(ctx, f.conn.ID, "99999"))
		}},
	} {
		err := tc.call()
		code, message, ok := errorsx.PublicDetails(err)
		if !ok || code != tc.code || (tc.cause != nil && !errors.Is(err, tc.cause)) || message == "" {
			t.Errorf("%s = %v (code %q), want %s with cause %v", tc.name, err, code, tc.code, tc.cause)
		}
	}
	_, err = f.a.MintTrackerAttachmentUploadTicket(ctx, f.conn.ID, "10101", strings.Repeat("n", 1025), 1)
	if _, message, _ := errorsx.PublicDetails(err); message != "nothing was sent: a file name is at most 1024 bytes, and this one is 1025; rename the file" {
		t.Errorf("a long file name says %q", message)
	}
	// A name Windows holds: 90 CJK characters and .txt, 94 UTF-16 units
	// and 274 bytes.
	if _, err := f.a.MintTrackerAttachmentUploadTicket(ctx, f.conn.ID, "10101", strings.Repeat("文", 90)+".txt", 1); err != nil {
		t.Errorf("a 274-byte name Windows holds: %v", err)
	}
	if _, err := f.a.SubscribeTrackerTicket(ctx, f.conn.ID, strings.Repeat("1", 65), true); err == nil ||
		!strings.Contains(err.Error(), "an issue ID is at most 64 bytes, and this one is 65") {
		t.Errorf("a long issue ID's sentence: %v", err)
	}
	if _, err := f.a.AddTrackerComment(ctx, f.conn.ID, trackerapp.CommentRequest{Issue: "10101", Markdown: "x", Audience: "everyone"}); err == nil ||
		!strings.Contains(err.Error(), `comment audience "everyone" is not public or internal`) {
		t.Errorf("an invalid request's sentence does not say what was wrong: %v", err)
	}
}

// A client's ticket subscriptions end with its connection, as a board's
// do.
func TestTrackerTicketSubscriptionsFollowTheirClient(t *testing.T) {
	t.Parallel()
	f := newTrackerBoardFixture(t)
	for range 3 {
		ctx, conn := newTestConnection(t, transport.ConnPrincipal{})
		sub, err := f.a.SubscribeTrackerTicket(ctx, f.conn.ID, "10101", true)
		if err != nil {
			t.Fatal(err)
		}
		if !conn.Bound(trackerSubscriptionCleanupKey(sub.ID)) {
			t.Fatal("the ticket subscription is not tied to its connection")
		}
		conn.RunCleanups()
	}
	if _, handles, _, pumps := f.a.trackers.SubscriptionCountsForTesting(); handles+pumps != 0 {
		t.Fatalf("after three clients came and went: %d ticket handles and %d ticket pumps", handles, pumps)
	}
}

// trackerTicketConnectionTie is SubscribeTrackerTicket's row of
// connectionTieChecks: a handle on the ticket's pump, released by any
// caller.
func trackerTicketConnectionTie(t *testing.T) connectionTie {
	f := newTrackerBoardFixture(t)
	return connectionTie{
		open: func(ctx context.Context) (string, error) {
			sub, err := f.a.SubscribeTrackerTicket(ctx, f.conn.ID, "10101", true)
			return sub.ID, err
		},
		// The handle and the pump it alone keeps.
		held: func() int {
			_, handles, _, pumps := f.a.trackers.SubscriptionCountsForTesting()
			return handles + pumps
		},
		unit:      2,
		release:   f.a.ReleaseTrackerSubscription,
		anyCaller: true,
		key:       trackerSubscriptionCleanupKey,
	}
}

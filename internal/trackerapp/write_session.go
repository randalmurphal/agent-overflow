package trackerapp

import (
	"context"
	"errors"
	"fmt"

	"agent-overflow/internal/tracker"
)

// A write session is the writes one agent call makes (WithWrites), and
// what that call read to plan them. It keeps three things:
//
//   - each issue as the call's full read of it found it (tracker
//     Detail.Read), until the call sends anything to the issue: the
//     first move or edit of the issue takes it (Move, Edit), so the
//     provider does not read again what the call read; a later write
//     reads as any write does;
//   - the project a key lookup made in the session (Lookup) found each
//     issue in: a comment the session adds to the issue is judged on it
//     (AddComment, tracker CommentInput.Project), so a caller that waits
//     for a person looks the issues up after the wait. The plan's reads
//     (IssueCards, IssueDetails) keep none: they are made before a wait;
//   - the refresh each write owes (Service.write): a write in a session
//     sends and answers its parts at once, and EndWrites refreshes every
//     resource the session's writes named, one refresh per connection,
//     then records the agent's writes that wait on it.
//
// A comment's or a create's record is claimed before it is sent and
// settled when its send returns, in a session as outside one
// (agent_writes.go): the repeat record never waits on the refresh. A
// create is refreshed at once in a session too, since its placement on
// its board is read with its own refresh. A person's write has no
// session: its refresh runs before it returns, and its provider reads
// what it checks.

// sessionKey is the context key of a write session.
type sessionKey struct{}

// writeSession is one call's session: the reads its writes may take and
// the projects its lookups found, by connection and issue, the issues it
// sent anything to, and the refreshes
// owed, by connection in the order the writes were made.
type writeSession struct {
	reads    map[ticketKey]tracker.IssueRead
	projects map[ticketKey]tracker.ProjectID
	sentTo   map[ticketKey]bool
	owed     map[string]*owedRefresh
	ordered  []string
}

// owedRefresh is what a connection's writes in a session owe its
// refresh: the resources they named, and the agent records of the writes
// whose record waits on the refreshed cards.
type owedRefresh struct {
	resources []tracker.Resource
	records   []owedRecord
}

// owedRecord is a write's agent record and its result.
type owedRecord struct {
	rec *agentRecord
	res tracker.Result
}

// WithWrites is ctx for the writes of one call and the reads that plan
// them, sharing one session; ctx's own when it carries one. The caller
// ends it with Service.EndWrites once its writes are sent.
func WithWrites(ctx context.Context) context.Context {
	if sessionOf(ctx) != nil {
		return ctx
	}
	return context.WithValue(ctx, sessionKey{}, &writeSession{
		reads: map[ticketKey]tracker.IssueRead{}, projects: map[ticketKey]tracker.ProjectID{}, sentTo: map[ticketKey]bool{}, owed: map[string]*owedRefresh{},
	})
}

// sessionOf is ctx's write session, nil when it carries none.
func sessionOf(ctx context.Context) *writeSession {
	sess, _ := ctx.Value(sessionKey{}).(*writeSession)
	return sess
}

// keepRead keeps d's read of issue on connection id for the first write
// to it, when ctx carries a session and nothing was sent to the issue in
// it yet.
func (sess *writeSession) keepRead(id string, d tracker.Detail) {
	if sess == nil || d.Read == nil {
		return
	}
	if k := (ticketKey{connection: id, issue: d.Card.ID}); !sess.sentTo[k] {
		sess.reads[k] = d.Read
	}
}

// read is the session's read of issue on connection id, nil when it
// holds none or something was sent to the issue since.
func (sess *writeSession) read(id string, issue tracker.IssueID) tracker.IssueRead {
	if sess == nil {
		return nil
	}
	return sess.reads[ticketKey{connection: id, issue: issue}]
}

// keepProjects keeps the project of each card found, on connection id,
// when ctx carries a session.
func (sess *writeSession) keepProjects(id string, found []tracker.KeyMatch) {
	if sess == nil {
		return
	}
	for _, m := range found {
		sess.projects[ticketKey{connection: id, issue: m.Card.ID}] = m.Card.Project
	}
}

// project is the project the session's last key lookup found issue on
// connection id in, empty when none found it.
func (sess *writeSession) project(id string, issue tracker.IssueID) tracker.ProjectID {
	if sess == nil {
		return ""
	}
	return sess.projects[ticketKey{connection: id, issue: issue}]
}

// sent records that res was sent on connection id: no later write takes
// a read of the issues its parts name.
func (sess *writeSession) sent(id string, res tracker.Result) {
	if sess == nil {
		return
	}
	for _, issue := range partIssues(res) {
		k := ticketKey{connection: id, issue: issue}
		sess.sentTo[k] = true
		delete(sess.reads, k)
	}
}

// owe adds a write's resources and its record to what connection id's
// refresh owes.
func (sess *writeSession) owe(id string, resources []tracker.Resource, rec *agentRecord, res tracker.Result) {
	o := sess.owed[id]
	if o == nil {
		o = &owedRefresh{}
		sess.owed[id] = o
		sess.ordered = append(sess.ordered, id)
	}
	o.resources = append(o.resources, resources...)
	if rec != nil {
		o.records = append(o.records, owedRecord{rec: rec, res: res})
	}
}

// EndWrites runs the refresh ctx's session owes, one per connection its
// writes were made on, and records the agent's writes that waited on it,
// each with its cards' update times after the refresh. A connection that
// cannot be refreshed (turned off, removed) records them with none. It
// returns what could not be refreshed, and why, as an error; the writes
// stand either way. A ctx with no session, or a session whose writes sent
// nothing, ends with nothing read.
func (s *Service) EndWrites(ctx context.Context) error {
	sess := sessionOf(ctx)
	if sess == nil {
		return nil
	}
	var errs []error
	for _, id := range sess.ordered {
		o := sess.owed[id]
		delete(sess.owed, id)
		if err := s.endWrites(ctx, id, o); err != nil {
			errs = append(errs, err)
		}
	}
	sess.ordered = nil
	return errors.Join(errs...)
}

// endWrites is EndWrites on connection id.
func (s *Service) endWrites(ctx context.Context, id string, o *owedRefresh) error {
	r, err := s.interact(ctx, id)
	if err != nil {
		for _, w := range o.records {
			s.finishAgentWrite(w.rec, w.res, nil)
		}
		return fmt.Errorf("trackerapp: refresh the writes on %s: %w", id, err)
	}
	defer r.finish()
	f := r.refresh(o.resources, newIssue{})
	for _, w := range o.records {
		s.finishAgentWrite(w.rec, w.res, s.cardTimes(r.root, partIssues(w.res)))
	}
	ended := f.ended || !r.inUse()
	if !ended && r.ctx.Err() == nil {
		// A settlement Stop overtook changes nothing: the writes stand.
		_ = r.settleCalls(f.lastCallError())
	}
	if len(f.pumps) > 0 {
		s.settleTickets(ctx, id, f.pumps, f.began, f.reads[:len(f.pumps)], f.details, f.detailsErr, millis(s.now()))
	}
	if ended || len(f.stale) == 0 {
		return nil
	}
	var errs []error
	for _, v := range s.staleViews(r.row.Kind, f.stale) {
		errs = append(errs, fmt.Errorf("trackerapp: %d resources of %s were not read again after the writes: %s", len(v.Resources), id, v.Failure.Message))
	}
	return errors.Join(errs...)
}

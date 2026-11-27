package trackerapp

import (
	"errors"
	"fmt"
	"slices"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
)

// evidenceBatch is the most issues one evidence read names: half of what
// Jira's bulk fetch takes with comments (100). A batch whose answer is
// over the transport's bound is read in smaller ones (trackerapi.Halved).
// A unit reads the one page its listing returns (store ListMirrorActsDue,
// 200 issues), so at most 4 reads.
const evidenceBatch = 50

// evidence is the evidence unit (SyncLinked): who commented on and logged
// time on the tickets credit counts, for the boards of every project of
// the connection that counts credit (creditBoards), read in batches of
// evidenceBatch, one listing's page a tick (More). Which sources of which
// tickets are due, from when, and in what order, is the store's rule
// (store ListMirrorActsDue). Each source a read asks for ends the tick
// stored with its receipt, or recorded as a failed attempt with why when
// it failed for its ticket alone, which keeps it off the listing for a
// day; so each tick's listing reaches the tickets behind the last, and
// More is the listing's. The attempts a tick records are reported to the
// loop's failure report. A failure every ticket shares stores what the
// read returned before it and fails the tick. With no project counting
// credit, or a tracker credit does not count (tracker.Capabilities.Evidence),
// it lists and reads nothing.
//
// A read across a change of the connection's time source stores no
// worklog (store ReplaceMirrorActs), its failure included when the change
// closed the client it read through, and reports More: the next tick
// reads under the new source with a provider built for it.
func (r *run) evidence() (UnitResult, error) {
	if !r.p.Capabilities().Evidence {
		return UnitResult{}, nil
	}
	boards, err := r.s.creditBoards(r.row)
	if err != nil || len(boards) == 0 {
		return UnitResult{}, err
	}
	due, more, err := r.s.store.ListMirrorActsDue(r.root, boards, millis(r.window()), millis(r.s.now()))
	if err != nil {
		return UnitResult{}, fmt.Errorf("trackerapp: list the tickets whose evidence is due: %w", err)
	}
	var log changeLog
	var failed []tracker.SourceFailure
	defer func() {
		if len(failed) > 0 {
			r.s.loopFailure(fmt.Errorf("trackerapp: credit could not read %d tickets' comments or worklogs of %s, read again in a day; the first: %w",
				len(failed), r.row.ID, failed[0].Err))
		}
	}()
	for batch := range slices.Chunk(due, evidenceBatch) {
		changed, tried, err := r.evidenceOf(batch)
		log.add(Changes{Catalog: changed})
		failed = append(failed, tried...)
		switch {
		case errors.Is(err, store.ErrTrackerCacheSourceChanged):
			return UnitResult{Changes: log.changes(), More: true}, nil
		case err != nil:
			if moved, readErr := r.timeSourceMoved(); readErr != nil || moved {
				return UnitResult{Changes: log.changes(), More: moved}, readErr
			}
			return UnitResult{Changes: log.changes()}, err
		}
	}
	return UnitResult{Changes: log.changes(), More: more}, nil
}

// timeSourceMoved reports that the connection's time source changed since
// the run began (TrackerConnection.TimeSourceGen): a read that failed
// because the change closed its client is read again under the new one.
func (r *run) timeSourceMoved() (bool, error) {
	row, err := r.s.store.GetTrackerConnection(r.row.ID)
	if err != nil {
		return false, fmt.Errorf("trackerapp: read connection %s: %w", r.row.ID, err)
	}
	return row.TimeSourceGen != r.row.TimeSourceGen, nil
}

// evidenceOf reads the due sources of the tickets of due and stores what
// it read and the sources that failed for their ticket alone, which it
// returns, and reports whether the catalog changed. A failure of the
// whole read is returned after what came before it is stored.
func (r *run) evidenceOf(due []store.MirrorActsDue) (changed bool, failed []tracker.SourceFailure, _ error) {
	q := tracker.EvidenceQuery{Issues: make([]tracker.EvidenceIssue, len(due))}
	for i, d := range due {
		q.Issues[i] = tracker.EvidenceIssue{Issue: tracker.IssueID(d.IssueID), Comments: d.Comments, Worklogs: d.Worklogs, WorklogsFrom: timeOf(d.WorklogsFrom)}
	}
	read, err := r.beginRead()
	if err != nil {
		return false, nil, err
	}
	r.calls++
	ev, readErr := r.p.Evidence(r.ctx, q)
	if readErr != nil {
		readErr = fmt.Errorf("trackerapp: read who worked on %d tickets: %w", len(due), call(readErr))
	}
	catalog, err := mirrorCatalog(ev.Catalog)
	if err != nil {
		return false, nil, errors.Join(readErr, err)
	}
	from := make(map[string]int64, len(due))
	for _, d := range due {
		from[d.IssueID] = d.WorklogsFrom
	}
	acts := make([]store.MirrorIssueActs, 0, len(ev.Read))
	for _, se := range ev.Read {
		in := store.MirrorIssueActs{IssueID: string(se.Issue), Source: mirrorActSource(se.Source), ReadFor: millis(se.Updated), Acts: mirrorActs(se.Acts)}
		if se.Source == tracker.EvidenceWorklogs {
			in.From = from[in.IssueID]
		}
		acts = append(acts, in)
	}
	tried := make([]store.MirrorActsAttempt, len(ev.Failed))
	for i, f := range ev.Failed {
		tried[i] = store.MirrorActsAttempt{IssueID: string(f.Issue), Source: mirrorActSource(f.Source), Reason: f.Err.Error()}
	}
	if len(acts)+len(tried) == 0 {
		return false, nil, readErr
	}
	catalogWrite, err := r.s.store.ReplaceMirrorActs(r.root, r.row.TimeSourceGen, read, acts, tried, catalog)
	r.told("", written{Changes: Changes{Catalog: catalogWrite.Changed}, links: catalogWrite.Threads})
	if err != nil {
		err = fmt.Errorf("trackerapp: store who worked on %d tickets: %w", len(due), err)
	}
	if err == nil || errors.Is(err, store.ErrTrackerCacheSourceChanged) {
		failed = ev.Failed
	}
	return catalogWrite.Changed, failed, errors.Join(err, readErr)
}

// mirrorActSource is source as the mirror stores it.
func mirrorActSource(source tracker.EvidenceSource) int {
	if source == tracker.EvidenceComments {
		return store.MirrorActComment
	}
	return store.MirrorActWorklog
}

// mirrorActs is acts as the mirror stores them.
func mirrorActs(acts []tracker.Act) []store.MirrorAct {
	out := make([]store.MirrorAct, len(acts))
	for i, a := range acts {
		out[i] = store.MirrorAct{ID: a.ID, Person: string(a.Author), At: millis(a.At), Seconds: int64(a.Spent.Seconds())}
	}
	return out
}

// creditBoards are the boards of connection row that a project counting
// credit lists (heldSetup.countsCredit), each with the regrouping that
// project counts row's site by; a board two such projects list is named
// once for each. It reads the connection's listings alone, and each
// project's setup once. A project's setup that cannot be read fails it.
func (s *Service) creditBoards(row store.TrackerConnection) ([]store.MirrorActsBoard, error) {
	listed, err := s.store.ListConnectionProjectBoards(row.ID)
	if err != nil {
		return nil, fmt.Errorf("trackerapp: read the boards projects list of %s: %w", row.ID, err)
	}
	site := siteKey{row.Kind, row.Site}
	type counting struct {
		counts bool
		groups StatusGroups
	}
	projects := map[string]counting{}
	var out []store.MirrorActsBoard
	for _, b := range listed {
		c, ok := projects[b.ProjectID]
		if !ok {
			s.setupMu.Lock()
			held, err := s.heldSetupLocked(b.ProjectSlug)
			if err == nil && held.countsCredit() {
				c = counting{counts: true, groups: held.counting(site)}
			}
			s.setupMu.Unlock()
			if err != nil {
				return nil, fmt.Errorf("trackerapp: the credit setup of project %s: %w", b.ProjectID, err)
			}
			projects[b.ProjectID] = c
		}
		if c.counts {
			out = append(out, store.MirrorActsBoard{BoardID: b.BoardID, Regroup: c.groups.regroup})
		}
	}
	return out, nil
}

package trackerapp

import (
	"errors"
	"fmt"
	"slices"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
)

// evidenceBatch is the most issues one evidence read names: half of what
// Jira's bulk fetch takes with comments (100), so a batch of issues with
// long comment threads stays well inside the transport's bound on an
// answer (trackerapi's 8 MiB). A unit reads the one page its listing
// returns (store ListMirrorActsDue, 200 issues), so at most 4 reads.
const evidenceBatch = 50

// evidence is the evidence unit (SyncLinked): who commented on and logged
// time on the tickets credit counts, for the boards of every project of
// the connection that counts credit (creditBoards), read in batches of
// evidenceBatch, one listing's page a tick (More). Which tickets are due, and
// in what order, is the store's rule (store ListMirrorActsDue): a ticket is
// due until a read for its card's update stored a receipt of its comments
// and of its worklogs. A support ticket's worklogs are read from the
// window's start, every other ticket's whole. A ticket the tracker does
// not return stores nothing and stays due. With no project counting
// credit, or a tracker credit does not count (tracker.Capabilities.Evidence),
// it lists and reads nothing.
//
// A read whose worklogs the store refuses because the connection's time
// source changed during it stores nothing and reports More: the next tick
// reads under the new source with a provider built for it.
func (r *run) evidence() (UnitResult, error) {
	if !r.p.Capabilities().Evidence {
		return UnitResult{}, nil
	}
	boards, err := r.s.creditBoards(r.row)
	if err != nil || len(boards) == 0 {
		return UnitResult{}, err
	}
	window := r.window()
	due, more, err := r.s.store.ListMirrorActsDue(r.root, boards, millis(window))
	if err != nil {
		return UnitResult{}, fmt.Errorf("trackerapp: list the tickets whose evidence is due: %w", err)
	}
	var log changeLog
	for batch := range slices.Chunk(due, evidenceBatch) {
		changed, err := r.evidenceOf(batch, window.UnixMilli())
		log.add(Changes{Catalog: changed})
		switch {
		case errors.Is(err, store.ErrTrackerCacheSourceChanged):
			return UnitResult{Changes: log.changes(), More: true}, nil
		case err != nil:
			return UnitResult{Changes: log.changes()}, err
		}
	}
	return UnitResult{Changes: log.changes(), More: more}, nil
}

// evidenceOf reads and stores the evidence of the tickets of due, a
// support ticket's worklogs from windowMs, and reports whether the catalog
// changed.
func (r *run) evidenceOf(due []store.MirrorActsDue, windowMs int64) (bool, error) {
	q := tracker.EvidenceQuery{Issues: make([]tracker.EvidenceIssue, len(due))}
	for i, d := range due {
		q.Issues[i] = tracker.EvidenceIssue{Issue: tracker.IssueID(d.IssueID)}
		if d.Support {
			q.Issues[i].WorklogsFrom = timeOf(windowMs)
		}
	}
	read, err := r.beginRead()
	if err != nil {
		return false, err
	}
	r.calls++
	ev, err := r.p.Evidence(r.ctx, q)
	if err != nil {
		return false, fmt.Errorf("trackerapp: read who worked on %d tickets: %w", len(due), call(err))
	}
	catalog, err := mirrorCatalog(ev.Catalog)
	if err != nil {
		return false, err
	}
	var acts []store.MirrorIssueActs
	for _, ie := range ev.Issues {
		readFor := millis(ie.Updated)
		acts = append(acts, store.MirrorIssueActs{IssueID: string(ie.Issue), Source: store.MirrorActComment, ReadFor: readFor, Acts: mirrorActs(ie.Comments)},
			store.MirrorIssueActs{IssueID: string(ie.Issue), Source: store.MirrorActWorklog, ReadFor: readFor, Acts: mirrorActs(ie.Worklogs)})
	}
	_, catalogWrite, err := r.s.store.ReplaceMirrorActs(r.root, r.row.TimeSourceGen, read, acts, catalog)
	r.told("", written{Changes: Changes{Catalog: catalogWrite.Changed}, links: catalogWrite.Threads})
	if err != nil {
		return catalogWrite.Changed, fmt.Errorf("trackerapp: store who worked on %d tickets: %w", len(ev.Issues), err)
	}
	return catalogWrite.Changed, nil
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
// once for each. A project's setup that cannot be read fails it.
func (s *Service) creditBoards(row store.TrackerConnection) ([]store.MirrorActsBoard, error) {
	choices, err := s.boardChoices("")
	if err != nil {
		return nil, err
	}
	site := siteKey{row.Kind, row.Site}
	slugs := map[string]string{}
	var out []store.MirrorActsBoard
	for _, c := range choices {
		if c.Connection != row.ID {
			continue
		}
		slug, ok := slugs[c.Project]
		if !ok {
			if slug, err = s.projectSlug(c.Project); err != nil {
				return nil, err
			}
			slugs[c.Project] = slug
		}
		s.setupMu.Lock()
		held, err := s.heldSetupLocked(slug)
		var groups StatusGroups
		counts := err == nil && held.countsCredit()
		if counts {
			groups = held.counting(site)
		}
		s.setupMu.Unlock()
		if err != nil {
			return nil, fmt.Errorf("trackerapp: the credit setup of project %s: %w", c.Project, err)
		}
		if counts {
			out = append(out, store.MirrorActsBoard{BoardID: c.Board, Regroup: groups.regroup})
		}
	}
	return out, nil
}

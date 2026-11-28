package trackerapp

import (
	"database/sql"
	"errors"
	"fmt"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
)

// Credit from the forge: a board whose project's credit rules count
// comments or approvals (StatusGroups.forge) reads the approvals and
// comments on its tickets' pull requests from the forge pull request
// history the app keeps (store tracker_cache_credit_forge.go). The
// service owns the two things that history needs of a connection: who
// each forge account is in the tracker (run.forgePeople), and telling
// the boards to count again when what they read of it changed
// (Service.ForgeCreditChanged).

// forgeBoards are the boards of connection row whose credit reads the
// forge history (StatusGroups.forge), each with its project's
// regrouping.
func (s *Service) forgeBoards(row store.TrackerConnection) ([]store.MirrorActsBoard, error) {
	return s.countingBoards(row, func(g StatusGroups) bool { return g.credit && g.forge })
}

// forgePeople is the people unit (SyncLinked): it matches the forge
// accounts whose match is due (store ListMirrorForgePeopleDue) to the
// tracker's people, one listing's page a tick (More). The account whose
// token read the repository is the connection's own verified account;
// any other is the one person whose name in the tracker is the name the
// forge gives it (Provider.PersonNamed), and nobody when the forge gives
// none, no person has the name or several do. A match stands for a day
// and until the forge names the account otherwise. With no board whose
// credit reads the forge, or a tracker credit does not count, it lists
// and matches nothing.
//
// A lookup that fails stores the matches made before it and fails the
// tick. A match that names another person than before has the
// connection's boards count again (tellRecount), their totals advanced
// with the write.
func (r *run) forgePeople() (UnitResult, error) {
	if !r.p.Capabilities().Evidence {
		return UnitResult{}, nil
	}
	boards, err := r.s.forgeBoards(r.row)
	if err != nil || len(boards) == 0 {
		return UnitResult{}, err
	}
	now := millis(r.s.now())
	due, more, err := r.s.store.ListMirrorForgePeopleDue(r.root, now, r.row.VerifiedAccountID)
	if err != nil {
		return UnitResult{}, fmt.Errorf("trackerapp: list the forge accounts whose match is due: %w", err)
	}
	if len(due) == 0 {
		return UnitResult{}, nil
	}
	read, err := r.beginRead()
	if err != nil {
		return UnitResult{}, err
	}
	matches := make([]store.MirrorForgeMatch, 0, len(due))
	var named tracker.Catalog
	var readErr error
	for _, d := range due {
		m := store.MirrorForgeMatch{MirrorForgePerson: d.MirrorForgePerson}
		switch {
		case d.Viewer:
			m.Person = r.row.VerifiedAccountID
		case d.Name != "":
			r.calls++
			person, ok, err := r.p.PersonNamed(r.ctx, d.Name)
			if err != nil {
				readErr = fmt.Errorf("trackerapp: find the person a forge account is: %w", call(err))
			} else if ok {
				m.Person = string(person.ID)
				named.Users = append(named.Users, person)
			}
		}
		if readErr != nil {
			break
		}
		matches = append(matches, m)
	}
	catalog, err := mirrorCatalog(named)
	if err != nil {
		return UnitResult{}, errors.Join(readErr, err)
	}
	ids := make([]string, len(boards))
	for i, b := range boards {
		ids[i] = b.BoardID
	}
	changed, err := r.s.store.SetMirrorForgePeople(r.root, read, matches, catalog, now, ids)
	if err != nil {
		return UnitResult{}, errors.Join(readErr, fmt.Errorf("trackerapp: store who %d forge accounts are: %w", len(matches), err))
	}
	if changed {
		r.s.tellRecount(r.row.ID)
	}
	return UnitResult{More: more && readErr == nil}, readErr
}

// ForgeCreditChanged tells connection id's boards whose credit reads the
// forge history that it changed, as the app's forge reads report it
// (store ForgeHistoryChange): with all, what a repository covers or says
// of its failure, or which repositories the connection reads, so every
// such board counts again; else the pull requests of the tickets keys
// name, so the boards holding one whose credit can read them do (store
// AdvanceMirrorForgeCredit). Each board told has its totals advanced
// first, so the Backlog's pace is read again with its dashboards. A
// connection with no such board (a tracker credit does not count has
// none, heldSetup.counting) or no mirror (one that is off has none) is
// told nothing.
func (s *Service) ForgeCreditChanged(id string, keys []string, all bool) error {
	if err := s.enter(); err != nil {
		return err
	}
	defer s.leave()
	row, err := s.store.GetTrackerConnection(id)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil
	case err != nil:
		return fmt.Errorf("trackerapp: read connection %s: %w", id, err)
	}
	boards, err := s.forgeBoards(row)
	if err != nil || len(boards) == 0 {
		return err
	}
	s.opMu.Lock()
	root, ok, err := s.store.GetMirrorRoot(id)
	gen := s.gen(id)
	s.opMu.Unlock()
	if err != nil {
		return fmt.Errorf("trackerapp: the mirror of connection %s: %w", id, err)
	}
	if !ok {
		return nil
	}
	issues, err := s.store.AdvanceMirrorForgeCredit(root, boards, keys, all)
	switch {
	case errors.Is(err, store.ErrTrackerCacheGone):
		return nil
	case err != nil:
		return fmt.Errorf("trackerapp: tell the boards of %s their forge credit changed: %w", id, err)
	case all:
		s.tellRecount(id)
	case len(issues) > 0:
		s.publish(root, gen, id, Changes{Evidence: issueIDs(issues)}, nil, nil)
	}
	return nil
}

// evidenceFailure is what the credit sentence says failed of the reads
// credit counts on root, for a project that counts it by groups: why the
// evidence unit's last read failed as a whole, and, when a rule counts
// comments or approvals, the repositories whose last read of their pull
// requests failed, the first by its path named with its failure and the
// rest counted. "" while none failed.
func (s *Service) evidenceFailure(root int64, groups StatusGroups) (string, error) {
	failure, err := s.store.GetMirrorEvidenceFailure(root)
	if err != nil || !groups.forge {
		return failure, err
	}
	failed, err := s.store.MirrorForgeFailures(root)
	if err != nil || len(failed) == 0 {
		return failure, err
	}
	more := ""
	switch n := len(failed) - 1; {
	case n == 1:
		more = " and 1 more repository"
	case n > 1:
		more = fmt.Sprintf(" and %d more repositories", n)
	}
	forge := fmt.Sprintf("Pull requests of %s%s could not be read: %s", failed[0].Repo, more, failed[0].Failure)
	if failure == "" {
		return forge, nil
	}
	return failure + " " + forge, nil
}

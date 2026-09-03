package projectapp

import "agent-overflow/internal/store"

// BackfillIdentity derives the repository identity of every project row that
// has neither half yet, and hands each row it actually moved to persist.
//
// One pass, meant to be run once per boot. Rows written before migration v83,
// and rows created while no identity deriver was wired, are the whole
// population; a project that already has an answer is skipped without a git
// subprocess, so the second boot after an upgrade costs nothing. There is no
// polling and no retry: a checkout that gains an origin later is re-derived by
// the next boot's pass, which is the same window the repo-meta cache already
// accepts for the classification derived from it.
//
// ARCHIVED ROWS ARE INCLUDED. An archived project can be unarchived at any
// time, and skipping it here would leave it the one entry that never merges
// across machines.
//
// A row whose path no longer exists, or was never a repository, derives ("",
// "") and is skipped silently: that is not a failure, it is the answer, and
// writing it back would only restate the empty values already stored.
//
// persist is called once per changed row, in list order, on the caller's
// goroutine — `internal/app` broadcasts it on `project:updated` so a client
// that loaded its sidebar before the pass converges without a refresh. A nil
// persist runs the writes and announces nothing.
func (s *Service) BackfillIdentity(persist func(row store.Project)) error {
	database, err := s.database("backfill project identity")
	if err != nil {
		return err
	}
	rows, err := database.ListAllProjects()
	if err != nil {
		return err
	}
	for _, row := range rows {
		if row.RemoteURL != "" || row.RootCommit != "" {
			continue
		}
		remoteURL, rootCommit := s.repoIdentity(row.Path)
		if remoteURL == "" && rootCommit == "" {
			continue
		}
		identified, changed, err := database.UpdateProjectIdentity(row.ID, remoteURL, rootCommit)
		if err != nil {
			return err
		}
		if changed && persist != nil {
			persist(identified)
		}
	}
	return nil
}

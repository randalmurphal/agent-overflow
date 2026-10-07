package projectapp

import (
	"context"

	"agent-overflow/internal/store"
)

// RefreshIdentity re-derives the repository identity of every project row and
// hands each row it actually moved to persist.
//
// One pass, meant to be run once per boot. Every row is read, not only rows
// with no identity yet: an `origin` added or changed since the row was
// written, and a read that failed last time, are both corrected here. Local Git reads do not walk history; forge lookups have a timeout and
// a bounded cache.
//
// ARCHIVED ROWS ARE INCLUDED. An archived project can be unarchived at any
// time, and skipping it here would leave it the one entry that never merges
// across machines.
//
// A failed read is recorded on the row (store.Project.IdentityError) rather
// than returned: it describes that checkout, and the pass continues with the
// rest. The returned error is a store failure or ctx ending the pass; a read
// ctx cut short is not recorded.
//
// persist is called once per changed row, in list order, on the caller's
// goroutine — `internal/app` broadcasts it on `project:updated` so a client
// that loaded its sidebar before the pass converges without a refresh. A nil
// persist runs the writes and announces nothing.
func (s *Service) RefreshIdentity(ctx context.Context, persist func(row store.Project)) error {
	database, err := s.database("refresh project identity")
	if err != nil {
		return err
	}
	if s.deps.Identity == nil {
		return nil
	}
	rows, err := database.ListAllProjects()
	if err != nil {
		return err
	}
	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			return err
		}
		identity, ok := s.repoIdentity(ctx, row.Path, projectIdentity(row))
		if !ok {
			return ctx.Err()
		}
		identified, changed, err := database.UpdateProjectIdentity(row.ID, identity)
		if err != nil {
			return err
		}
		if changed && persist != nil {
			persist(identified)
		}
	}
	return nil
}

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
// The returned IDs are the rows whose forge lookup failed because the forge
// or its CLI was unavailable. The caller retries them with RetryIdentity.
//
// persist is called once per changed row, in list order, on the caller's
// goroutine — `internal/app` broadcasts it on `project:updated` so a client
// that loaded its sidebar before the pass converges without a refresh. A nil
// persist runs the writes and announces nothing.
func (s *Service) RefreshIdentity(ctx context.Context, persist func(row store.Project)) ([]string, error) {
	return s.refreshIdentity(ctx, nil, persist)
}

// RetryIdentity is RefreshIdentity restricted to the rows in ids, for the
// rows an earlier pass returned. A row deleted since is skipped. It returns
// the rows whose forge is still unavailable.
func (s *Service) RetryIdentity(ctx context.Context, ids []string, persist func(row store.Project)) ([]string, error) {
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	return s.refreshIdentity(ctx, want, persist)
}

// refreshIdentity re-derives every row, or only the rows in want when it is
// non-nil.
func (s *Service) refreshIdentity(ctx context.Context, want map[string]bool, persist func(row store.Project)) ([]string, error) {
	database, err := s.database("refresh project identity")
	if err != nil {
		return nil, err
	}
	if s.deps.Identity == nil {
		return nil, nil
	}
	rows, err := database.ListAllProjects()
	if err != nil {
		return nil, err
	}
	var retry []string
	for _, row := range rows {
		if want != nil && !want[row.ID] {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		identity, read := s.repoIdentity(ctx, row.Path, projectIdentity(row))
		if !read.ok {
			return nil, ctx.Err()
		}
		identified, changed, err := database.UpdateProjectIdentity(row.ID, identity)
		if err != nil {
			return nil, err
		}
		if changed && persist != nil {
			persist(identified)
		}
		if read.retryable {
			retry = append(retry, row.ID)
		}
	}
	return retry, nil
}

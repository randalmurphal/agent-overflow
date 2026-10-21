package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// PRView is the last time the app opened a PR (migration v144): when, at
// which head, and the comment and commit counts the Pull requests page's
// list held for it then. A count the list had not read is nil.
type PRView struct {
	PRKey    string
	OpenedAt int64
	HeadSHA  string
	Comments *int
	Commits  *int
}

const prViewColumns = `pr_key, opened_at, head_sha, comments, commits`

func scanPRView(scan func(...any) error) (PRView, error) {
	var view PRView
	var comments, commits sql.NullInt64
	if err := scan(&view.PRKey, &view.OpenedAt, &view.HeadSHA, &comments, &commits); err != nil {
		return PRView{}, err
	}
	if comments.Valid {
		n := int(comments.Int64)
		view.Comments = &n
	}
	if commits.Valid {
		n := int(commits.Int64)
		view.Commits = &n
	}
	return view, nil
}

func nullableCount(n *int) any {
	if n == nil {
		return nil
	}
	return *n
}

// PutPRView records view as the PR's latest opening, replacing the one
// before, and returns the stored row.
func (s *Store) PutPRView(view PRView) (PRView, error) {
	switch {
	case view.PRKey == "" || len(view.PRKey) > MaxDiffReviewSourceKeyBytes || hasControlCharacter(view.PRKey):
		return PRView{}, fmt.Errorf("store: PR view key %q is invalid", view.PRKey)
	case view.OpenedAt <= 0:
		return PRView{}, fmt.Errorf("store: PR view of %s needs the time it was opened", view.PRKey)
	case (view.Comments != nil && *view.Comments < 0) || (view.Commits != nil && *view.Commits < 0):
		return PRView{}, fmt.Errorf("store: PR view of %s has a negative count", view.PRKey)
	}
	row := s.db.QueryRow(`INSERT INTO pr_views(`+prViewColumns+`) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(pr_key) DO UPDATE SET opened_at = excluded.opened_at, head_sha = excluded.head_sha,
		  comments = excluded.comments, commits = excluded.commits
		RETURNING `+prViewColumns,
		view.PRKey, view.OpenedAt, view.HeadSHA, nullableCount(view.Comments), nullableCount(view.Commits))
	stored, err := scanPRView(row.Scan)
	if err != nil {
		return PRView{}, fmt.Errorf("store: record view of %s: %w", view.PRKey, err)
	}
	return stored, nil
}

// ListPRViews reads the views of the PRs keys names, by key; a PR the app
// never opened is absent.
func (s *Store) ListPRViews(keys []string) (map[string]PRView, error) {
	out := make(map[string]PRView, len(keys))
	keys = uniqueNonEmptyStrings(keys)
	if len(keys) == 0 {
		return out, nil
	}
	encoded, err := json.Marshal(keys)
	if err != nil {
		return nil, fmt.Errorf("store: list PR views: %w", err)
	}
	rows, err := s.reader().Query(`SELECT `+prViewColumns+` FROM pr_views
		WHERE pr_key IN (SELECT value FROM json_each(?))`, string(encoded))
	if err != nil {
		return nil, fmt.Errorf("store: list PR views: %w", err)
	}
	for rows.Next() {
		view, err := scanPRView(rows.Scan)
		if err != nil {
			return nil, errors.Join(fmt.Errorf("store: scan PR view: %w", err), rows.Close())
		}
		out[view.PRKey] = view
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("store: list PR views: %w", err)
	}
	return out, nil
}

package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// The pull request history of forge repositories (migration v178): one
// source per read of a repository, its pull requests updated inside the
// window, who approved and commented on each, and the ticket keys each
// names. ApplyForgeHistory is its one writer and SetForgeHistorySources
// says which sources exist.

// ForgeHistoryClockMargin is how far a ticket's creation, on the
// tracker's clock, must be past a source's reach, on the forge's, for
// the source to cover it (ForgeHistorySource.Covers).
const ForgeHistoryClockMargin = 5 * time.Minute

// Forge history act kinds.
const (
	ForgeActApproved  = 1
	ForgeActCommented = 2
)

// ForgeHistoryRef names one source: the repository read (its forge, host
// and path, and git.RepoReference.Key as RepoKey) and the head it keeps.
// KeepHead is empty for a project's origin, whose every pull request is
// kept, and the fork's head identity (ForgeHistorySource.OwnHead of the
// fork's source) for a fork's parent.
type ForgeHistoryRef struct {
	RepoKey   string `json:"repoKey"`
	KeepHead  string `json:"keepHead"`
	Forge     string `json:"forge"`
	Host      string `json:"host"`
	Namespace string `json:"namespace"`
	Repo      string `json:"repo"`
}

// ForgeHistorySource is a source as stored.
type ForgeHistorySource struct {
	ForgeHistoryRef
	ID int64
	// OwnHead is the repository's head identity in the forge's terms, as
	// a pull request's HeadRepo names it; empty until a read lands.
	OwnHead string
	// ParentNamespace and ParentRepo are the repository the forge says it
	// is a fork of, on the same host; empty when it is none.
	ParentNamespace, ParentRepo string
	// Walk is the reader's state, JSON only the reader reads.
	Walk []byte
	// Reach is the forge time (Unix ms) back to which every update is
	// read, 0 back to the window's start; HasReach is false until a read
	// lands, and while the last one left a stream behind its newest
	// updates (ForgeHistoryRead.Behind).
	Reach    int64
	HasReach bool
	// Walked reports that the first full read is done.
	Walked bool
	// ReadAt is the wall time (Unix ms) the last read landed, failed or
	// not; 0 before one.
	ReadAt int64
	// FailureKind, Failure and FailureDetail are the last read's
	// failure, its kind, the sentence a person reads and the app's JSON
	// detail of it, at FailedAt; empty when it landed.
	FailureKind, Failure string
	FailureDetail        []byte
	FailedAt             int64
}

// Covers is the one rule for whether the source has read every pull
// request a ticket created at created (Unix ms, the tracker's clock) can
// have: the ticket was created at or after windowStart, the start of the
// window the history keeps, and the source has read every update back
// to ForgeHistoryClockMargin before it. A pull request can name a key
// only once the ticket exists, so each of the ticket's was updated at or
// after its creation.
func (s ForgeHistorySource) Covers(created, windowStart int64) bool {
	return s.HasReach && created >= windowStart && s.Reach <= created-ForgeHistoryClockMargin.Milliseconds()
}

// ForgeHistoryPR is a pull request as a read returns it. Times are Unix
// ms, 0 for one that has not happened. Keys are the ticket keys its title
// and source branch name (trackerapp.TicketKeys). With ActsWhole its Acts
// are all it has and replace what is stored; without, Acts are added to
// them.
type ForgeHistoryPR struct {
	Number     int64             `json:"n"`
	URL        string            `json:"url"`
	Title      string            `json:"title"`
	HeadRef    string            `json:"headRef"`
	HeadRepo   string            `json:"headRepo"`
	State      string            `json:"state"`
	Author     string            `json:"author"`
	AuthorName string            `json:"authorName"`
	CreatedAt  int64             `json:"created"`
	UpdatedAt  int64             `json:"updated"`
	MergedAt   int64             `json:"merged"`
	ClosedAt   int64             `json:"closed"`
	Keys       []string          `json:"-"`
	Acts       []ForgeHistoryAct `json:"-"`
	ActsWhole  bool              `json:"-"`
}

// ForgeHistoryAct is one person's approval or comment on a pull request,
// at its earliest time (Unix ms).
type ForgeHistoryAct struct {
	Number int64  `json:"n"`
	Kind   int    `json:"kind"`
	Login  string `json:"login"`
	Name   string `json:"name"`
	At     int64  `json:"at"`
}

// ForgeHistoryRead is what one read of a source applies.
type ForgeHistoryRead struct {
	Source ForgeHistoryRef
	// OwnHead, ParentNamespace and ParentRepo replace the stored ones
	// when OwnHead is set.
	OwnHead                     string
	ParentNamespace, ParentRepo string
	PRs                         []ForgeHistoryPR
	// Acts are acts of pull requests stored or in PRs, added to theirs
	// (an events feed's); one of a pull request neither holds, or one
	// the source does not keep, is dropped.
	Acts []ForgeHistoryAct
	// Walk, Reach and Walked replace the stored ones when Walk is set:
	// a read that read nothing leaves them. Behind is a read that left a
	// stream behind its newest updates, whose reach holds for nothing.
	Walk   []byte
	Reach  int64
	Walked bool
	Behind bool
	// ReadAt is the read's wall time (Unix ms).
	ReadAt int64
	// FailureKind, Failure and FailureDetail are the read's failure
	// (ForgeHistorySource); empty clears the stored one.
	FailureKind, Failure string
	FailureDetail        []byte
	// WindowStart is the start of the kept window (Unix ms): a pull
	// request with no update and no act since is pruned.
	WindowStart int64
}

// ErrForgeHistoryGone is a read of a source SetForgeHistorySources has
// removed since it began.
var ErrForgeHistoryGone = errors.New("store: the forge history source was removed")

var forgeHistoryStates = []string{"open", "draft", "merged", "closed"}

func (r ForgeHistoryRef) check() error {
	switch {
	case r.RepoKey == "" || hasControlCharacter(r.RepoKey) || hasControlCharacter(r.KeepHead):
		return fmt.Errorf("store: forge history source %q (keeps %q) is invalid", r.RepoKey, r.KeepHead)
	case r.Forge != "github" && r.Forge != "gitlab":
		return fmt.Errorf("store: forge history source %s has forge %q", r.RepoKey, r.Forge)
	case r.Host == "" || r.Namespace == "" || r.Repo == "":
		return fmt.Errorf("store: forge history source %s lacks its host or path", r.RepoKey)
	}
	return nil
}

func (r ForgeHistoryRead) check() error {
	if err := r.Source.check(); err != nil {
		return err
	}
	switch {
	case r.ReadAt <= 0 || r.WindowStart <= 0:
		return fmt.Errorf("store: forge history read of %s needs its time and window", r.Source.RepoKey)
	case (r.FailureKind == "") != (r.Failure == ""):
		return fmt.Errorf("store: forge history read of %s names a failure kind %q with sentence %q", r.Source.RepoKey, r.FailureKind, r.Failure)
	case len(r.FailureDetail) > 0 && (r.Failure == "" || !json.Valid(r.FailureDetail)):
		return fmt.Errorf("store: forge history read of %s has a failure detail that is not JSON of a failure", r.Source.RepoKey)
	case r.Walk != nil && !json.Valid(r.Walk):
		return fmt.Errorf("store: forge history read of %s has a walk that is not JSON", r.Source.RepoKey)
	case r.Walk != nil && r.Reach < 0:
		return fmt.Errorf("store: forge history read of %s reaches %d", r.Source.RepoKey, r.Reach)
	case (r.ParentNamespace == "") != (r.ParentRepo == ""):
		return fmt.Errorf("store: forge history read of %s names half a parent", r.Source.RepoKey)
	}
	for _, pr := range r.PRs {
		switch {
		case pr.Number <= 0 || pr.UpdatedAt <= 0 || pr.CreatedAt < 0 || pr.MergedAt < 0 || pr.ClosedAt < 0:
			return fmt.Errorf("store: forge history read of %s has pull request %d with invalid times", r.Source.RepoKey, pr.Number)
		case !slices.Contains(forgeHistoryStates, pr.State):
			return fmt.Errorf("store: forge history read of %s has pull request %d in state %q", r.Source.RepoKey, pr.Number, pr.State)
		case pr.URL == "":
			return fmt.Errorf("store: forge history read of %s has pull request %d with no address", r.Source.RepoKey, pr.Number)
		}
		for _, key := range pr.Keys {
			if key == "" || key != strings.ToUpper(key) {
				return fmt.Errorf("store: forge history read of %s has pull request %d naming key %q", r.Source.RepoKey, pr.Number, key)
			}
		}
		if err := checkForgeActs(r.Source.RepoKey, pr.Acts, pr.Number); err != nil {
			return err
		}
	}
	return checkForgeActs(r.Source.RepoKey, r.Acts, 0)
}

// checkForgeActs checks acts, each of pull request number unless it is 0.
func checkForgeActs(repoKey string, acts []ForgeHistoryAct, number int64) error {
	for _, a := range acts {
		if a.Number <= 0 || number != 0 && a.Number != number || a.Kind != ForgeActApproved && a.Kind != ForgeActCommented || a.Login == "" || a.At <= 0 {
			return fmt.Errorf("store: forge history read of %s has an invalid act %+v", repoKey, a)
		}
	}
	return nil
}

// SetForgeHistorySources makes the stored sources wanted: it adds the
// ones missing and removes every other, with its pull requests. It
// reports whether it changed any.
func (s *Store) SetForgeHistorySources(wanted []ForgeHistoryRef) (changed bool, err error) {
	for _, r := range wanted {
		if err := r.check(); err != nil {
			return false, err
		}
	}
	listed, err := jsonArray(wanted)
	if err != nil {
		return false, fmt.Errorf("store: encode forge history sources: %w", err)
	}
	const action = "store: set forge history sources"
	tx, err := s.db.Begin()
	if err != nil {
		return false, fmt.Errorf("%s: begin: %w", action, err)
	}
	defer rollbackTx(tx, action, &err)
	removed, err := tx.Exec(`DELETE FROM forge_history_sources WHERE NOT EXISTS (
		SELECT 1 FROM json_each(?1) w WHERE w.value ->> 'repoKey' = repo_key AND w.value ->> 'keepHead' = keep_head)`, listed)
	if err != nil {
		return false, fmt.Errorf("%s: remove: %w", action, err)
	}
	added, err := tx.Exec(`INSERT INTO forge_history_sources(repo_key, keep_head, forge, host, namespace, repo)
		SELECT w.value ->> 'repoKey', w.value ->> 'keepHead', w.value ->> 'forge', w.value ->> 'host', w.value ->> 'namespace', w.value ->> 'repo'
		FROM json_each(?1) w WHERE true
		ON CONFLICT(repo_key, keep_head) DO NOTHING`, listed)
	if err != nil {
		return false, fmt.Errorf("%s: add: %w", action, err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("%s: commit: %w", action, err)
	}
	gone, err := removed.RowsAffected()
	if err != nil {
		return false, err
	}
	made, err := added.RowsAffected()
	if err != nil {
		return false, err
	}
	return gone+made > 0, nil
}

const forgeHistorySourceColumns = `id, repo_key, keep_head, forge, host, namespace, repo, own_head, parent_namespace, parent_repo,
	walk, reach, walked, read_at, failure_kind, failure, failure_detail, failed_at`

func scanForgeHistorySource(row interface{ Scan(...any) error }) (ForgeHistorySource, error) {
	var src ForgeHistorySource
	var walk, detail string
	var reach sql.NullInt64
	err := row.Scan(&src.ID, &src.RepoKey, &src.KeepHead, &src.Forge, &src.Host, &src.Namespace, &src.Repo, &src.OwnHead,
		&src.ParentNamespace, &src.ParentRepo, &walk, &reach, &src.Walked, &src.ReadAt, &src.FailureKind, &src.Failure, &detail, &src.FailedAt)
	src.Walk, src.Reach, src.HasReach = []byte(walk), reach.Int64, reach.Valid
	if detail != "" {
		src.FailureDetail = []byte(detail)
	}
	return src, err
}

// ListForgeHistorySources reads every source, by repository key and kept
// head.
func (s *Store) ListForgeHistorySources() ([]ForgeHistorySource, error) {
	rows, err := s.reader().Query(`SELECT ` + forgeHistorySourceColumns + ` FROM forge_history_sources ORDER BY repo_key, keep_head`)
	if err != nil {
		return nil, fmt.Errorf("store: list forge history sources: %w", err)
	}
	var out []ForgeHistorySource
	for rows.Next() {
		src, err := scanForgeHistorySource(rows)
		if err != nil {
			return nil, errors.Join(fmt.Errorf("store: scan forge history source: %w", err), rows.Close())
		}
		out = append(out, src)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("store: list forge history sources: %w", err)
	}
	return out, nil
}

// ApplyForgeHistory applies one read of a source in one transaction: its
// pull requests replace the stored ones of their numbers, kept or not by
// the source's head, with their keys; each one's acts replace or add to
// its stored ones; the read's acts add to the stored pull requests'; the
// source's state and failure are recorded; and every pull request with no
// update and no act since the window's start is pruned. A re-read pull
// request with ActsWhole keeps only the acts it carries, so a withdrawn
// approval or a deleted comment leaves.
func (s *Store) ApplyForgeHistory(read ForgeHistoryRead) (err error) {
	if err := read.check(); err != nil {
		return err
	}
	prs, err := jsonArray(read.PRs)
	if err != nil {
		return fmt.Errorf("store: encode forge history pull requests: %w", err)
	}
	type keyRow struct {
		Key    string `json:"k"`
		Number int64  `json:"n"`
	}
	var keys []keyRow
	var whole []int64
	acts := slices.Clone(read.Acts)
	for _, pr := range read.PRs {
		for _, key := range pr.Keys {
			keys = append(keys, keyRow{key, pr.Number})
		}
		if pr.ActsWhole {
			whole = append(whole, pr.Number)
		}
		acts = append(acts, pr.Acts...)
	}
	keysJSON, err := jsonArray(keys)
	if err != nil {
		return err
	}
	wholeJSON, err := jsonArray(whole)
	if err != nil {
		return err
	}
	actsJSON, err := jsonArray(acts)
	if err != nil {
		return err
	}
	action := "store: apply forge history read of " + read.Source.RepoKey
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("%s: begin: %w", action, err)
	}
	defer rollbackTx(tx, action, &err)
	var id int64
	var keep string
	err = tx.QueryRow(`SELECT id, keep_head FROM forge_history_sources WHERE repo_key = ? AND keep_head = ?`, read.Source.RepoKey, read.Source.KeepHead).Scan(&id, &keep)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("%s (keeps %q): %w", action, read.Source.KeepHead, ErrForgeHistoryGone)
	case err != nil:
		return fmt.Errorf("%s: read source: %w", action, err)
	}
	steps := []struct {
		what string
		sql  string
		args []any
	}{
		{"store pull requests", forgeHistoryPRsSQL, []any{id, prs}},
		{"clear keys", `DELETE FROM forge_history_keys WHERE source_id = CAST(?1 AS INTEGER) AND number IN (SELECT p.value ->> 'n' FROM json_each(?2) p)`, []any{id, prs}},
		{"store keys", `INSERT INTO forge_history_keys(key, source_id, number)
			SELECT DISTINCT k.value ->> 'k', ?1, k.value ->> 'n' FROM json_each(?2) k
			JOIN forge_history_prs p ON p.source_id = CAST(?1 AS INTEGER) AND p.number = k.value ->> 'n' AND p.kept = 1`, []any{id, keysJSON}},
		{"clear acts", `DELETE FROM forge_history_acts WHERE source_id = CAST(?1 AS INTEGER) AND number IN (SELECT value FROM json_each(?2))`, []any{id, wholeJSON}},
		{"store acts", `INSERT INTO forge_history_acts(source_id, number, kind, login, name, at)
			SELECT ?1, a.value ->> 'n', a.value ->> 'kind', a.value ->> 'login', a.value ->> 'name', a.value ->> 'at' FROM json_each(?2) a
			JOIN forge_history_prs p ON p.source_id = CAST(?1 AS INTEGER) AND p.number = a.value ->> 'n' AND p.kept = 1 WHERE true
			ON CONFLICT(source_id, number, kind, login) DO UPDATE SET at = min(at, excluded.at), name = excluded.name`, []any{id, actsJSON}},
		{"prune", `DELETE FROM forge_history_prs WHERE source_id = CAST(?1 AS INTEGER) AND updated_at < CAST(?2 AS INTEGER) AND NOT EXISTS (
			SELECT 1 FROM forge_history_acts a WHERE a.source_id = CAST(?1 AS INTEGER) AND a.number = forge_history_prs.number AND a.at > CAST(?2 AS INTEGER) - 1)`, []any{id, read.WindowStart}},
	}
	for _, step := range steps {
		if _, err := tx.Exec(step.sql, step.args...); err != nil {
			return fmt.Errorf("%s: %s: %w", action, step.what, err)
		}
	}
	if read.OwnHead != "" {
		if _, err := tx.Exec(`UPDATE forge_history_sources SET own_head = ?2, parent_namespace = ?3, parent_repo = ?4 WHERE id = ?1`,
			id, read.OwnHead, read.ParentNamespace, read.ParentRepo); err != nil {
			return fmt.Errorf("%s: record the repository: %w", action, err)
		}
	}
	if read.Walk != nil {
		reach := sql.NullInt64{Int64: read.Reach, Valid: !read.Behind}
		if _, err := tx.Exec(`UPDATE forge_history_sources SET walk = ?2, reach = ?3, walked = walked OR ?4 WHERE id = ?1`,
			id, string(read.Walk), reach, read.Walked); err != nil {
			return fmt.Errorf("%s: record the walk: %w", action, err)
		}
	}
	failedAt := int64(0)
	if read.Failure != "" {
		failedAt = read.ReadAt
	}
	if _, err := tx.Exec(`UPDATE forge_history_sources SET read_at = ?2, failure_kind = ?3, failure = ?4, failure_detail = ?5, failed_at = ?6 WHERE id = ?1`,
		id, read.ReadAt, read.FailureKind, read.Failure, string(read.FailureDetail), failedAt); err != nil {
		return fmt.Errorf("%s: record the read: %w", action, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("%s: commit: %w", action, err)
	}
	return nil
}

// forgeHistoryPRsSQL stores a read's pull requests (?2) of source ?1: a
// pull request whose head the source does not keep is stored with its
// head and times only (kept 0).
const forgeHistoryPRsSQL = `INSERT INTO forge_history_prs(source_id, number, kept, url, title, head_ref, head_repo, state, author, author_name,
	created_at, updated_at, merged_at, closed_at)
SELECT ?1, x.v ->> 'n', x.kept,
	iif(x.kept, x.v ->> 'url', ''), iif(x.kept, x.v ->> 'title', ''), iif(x.kept, x.v ->> 'headRef', ''), x.v ->> 'headRepo',
	x.v ->> 'state', iif(x.kept, x.v ->> 'author', ''), iif(x.kept, x.v ->> 'authorName', ''),
	x.v ->> 'created', x.v ->> 'updated', x.v ->> 'merged', x.v ->> 'closed'
FROM (SELECT p.value AS v, s.keep_head = '' OR p.value ->> 'headRepo' = s.keep_head AS kept
	FROM json_each(?2) p JOIN forge_history_sources s ON s.id = CAST(?1 AS INTEGER)) x
WHERE true
ON CONFLICT(source_id, number) DO UPDATE SET kept = excluded.kept, url = excluded.url, title = excluded.title, head_ref = excluded.head_ref,
	head_repo = excluded.head_repo, state = excluded.state, author = excluded.author, author_name = excluded.author_name,
	created_at = excluded.created_at, updated_at = excluded.updated_at, merged_at = excluded.merged_at, closed_at = excluded.closed_at`

// ForgeHistoryRow is a kept pull request that names a key, with its
// source and acts.
type ForgeHistoryRow struct {
	Source ForgeHistoryRef
	ForgeHistoryPR
}

// ForgeHistoryByKey lists the kept pull requests that name key, by
// source and number.
func (s *Store) ForgeHistoryByKey(key string) ([]ForgeHistoryRow, error) {
	rows, err := s.reader().Query(`SELECT s.repo_key, s.keep_head, s.forge, s.host, s.namespace, s.repo,
		p.number, p.url, p.title, p.head_ref, p.head_repo, p.state, p.author, p.author_name, p.created_at, p.updated_at, p.merged_at, p.closed_at
		FROM forge_history_keys k
		JOIN forge_history_prs p ON p.source_id = k.source_id AND p.number = k.number
		JOIN forge_history_sources s ON s.id = k.source_id
		WHERE k.key = ?
		ORDER BY s.repo_key, s.keep_head, p.number`, strings.ToUpper(key))
	if err != nil {
		return nil, fmt.Errorf("store: read the forge history of %s: %w", key, err)
	}
	var out []ForgeHistoryRow
	for rows.Next() {
		var r ForgeHistoryRow
		if err := rows.Scan(&r.Source.RepoKey, &r.Source.KeepHead, &r.Source.Forge, &r.Source.Host, &r.Source.Namespace, &r.Source.Repo,
			&r.Number, &r.URL, &r.Title, &r.HeadRef, &r.HeadRepo, &r.State, &r.Author, &r.AuthorName,
			&r.CreatedAt, &r.UpdatedAt, &r.MergedAt, &r.ClosedAt); err != nil {
			return nil, errors.Join(fmt.Errorf("store: scan the forge history of %s: %w", key, err), rows.Close())
		}
		out = append(out, r)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("store: read the forge history of %s: %w", key, err)
	}
	return out, nil
}

// KnownForgeHistoryPRs reports which of numbers the source stores, kept
// or not, so a reader fetches only the pull requests an event names that
// it has never read.
func (s *Store) KnownForgeHistoryPRs(source ForgeHistoryRef, numbers []int64) (map[int64]bool, error) {
	listed, err := jsonArray(numbers)
	if err != nil {
		return nil, err
	}
	rows, err := s.reader().Query(`SELECT p.number FROM forge_history_sources s
		JOIN json_each(?3) n
		JOIN forge_history_prs p ON p.source_id = s.id AND p.number = n.value
		WHERE s.repo_key = ?1 AND s.keep_head = ?2`, source.RepoKey, source.KeepHead, listed)
	if err != nil {
		return nil, fmt.Errorf("store: read the known pull requests of %s: %w", source.RepoKey, err)
	}
	out := map[int64]bool{}
	for rows.Next() {
		var n int64
		if err := rows.Scan(&n); err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		out[n] = true
	}
	return out, errors.Join(rows.Err(), rows.Close())
}

// jsonArray is list as a JSON array, empty for a nil one, which
// json.Marshal writes as null and json_each reads as one row.
func jsonArray[T any](list []T) (string, error) {
	if list == nil {
		return "[]", nil
	}
	b, err := json.Marshal(list)
	return string(b), err
}

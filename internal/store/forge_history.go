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
// source per repository read, its pull requests updated inside the
// window, who approved and commented on each, and the ticket keys each
// names. ApplyForgeHistory is its one writer, SetForgeHistorySources says
// which sources exist, and ForgeHistoryForWork is the one read of which
// stored pull requests are a ticket's.

// ForgeHistoryClockMargin is how far a ticket's creation, on the
// tracker's clock, must be past a source's reach, on the forge's, for
// the source to cover it (ForgeHistorySource.Covers).
const ForgeHistoryClockMargin = 5 * time.Minute

// Forge history act kinds.
const (
	ForgeActApproved  = 1
	ForgeActCommented = 2
)

// ForgeHistoryRef names one source: a forge repository, by its forge,
// host and path, and git.RepoReference.Identity as RepoKey.
type ForgeHistoryRef struct {
	RepoKey   string `json:"repoKey"`
	Forge     string `json:"forge"`
	Host      string `json:"host"`
	Namespace string `json:"namespace"`
	Repo      string `json:"repo"`
}

// ForgeHistorySource is a source as stored.
type ForgeHistorySource struct {
	ForgeHistoryRef
	// ID is never reused: a read carries it, so one that began before its
	// source was removed lands in no source made again.
	ID int64
	// OwnHead is the repository's head identity in the forge's terms, as
	// a pull request's HeadRepo names it; empty until a read lands.
	OwnHead string
	// Parent is the repository the forge says it is a fork of, on the
	// same host; zero when it is none.
	Parent ForgeHistoryRef
	// Walk is the reader's state, JSON only the reader reads.
	Walk []byte
	// Reach is the forge time (Unix ms) back to which every update is
	// read, 0 back to the window's start; HasReach is false until a read
	// lands, and while a stream read oldest first has not reached the
	// present (ForgeHistoryRead.NoReach).
	Reach    int64
	HasReach bool
	// Owed reports that the walk has work left, which the next round
	// reads.
	Owed bool
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
// at its earliest time (Unix ms). Name is empty when the read did not
// name the person; a stored name is then kept.
type ForgeHistoryAct struct {
	Number int64  `json:"n"`
	Kind   int    `json:"kind"`
	Login  string `json:"login"`
	Name   string `json:"name"`
	At     int64  `json:"at"`
}

// ForgeHistoryRead is what one read of a source applies.
type ForgeHistoryRead struct {
	// SourceID and Source are the source the read began from: the write
	// lands only while that source is stored.
	SourceID int64
	Source   ForgeHistoryRef
	// OwnHead and Parent replace the stored ones when OwnHead is set.
	OwnHead string
	Parent  ForgeHistoryRef
	PRs     []ForgeHistoryPR
	// Acts are acts of pull requests stored or in PRs, added to theirs
	// (an events feed's, a comment list's). One whose pull request
	// neither holds fails the write: the reader resolves every act's pull
	// request before it moves past the act.
	Acts []ForgeHistoryAct
	// Walk, Reach, NoReach and Owed replace the stored ones when Walk is
	// set: a read that read nothing leaves them. NoReach is a read whose
	// reach holds for nothing (ForgeHistorySource.HasReach).
	Walk    []byte
	Reach   int64
	NoReach bool
	Owed    bool
	// ReadAt is the read's wall time (Unix ms), each pull request's
	// seen_at.
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
	case r.RepoKey == "" || hasControlCharacter(r.RepoKey):
		return fmt.Errorf("store: forge history source %q is invalid", r.RepoKey)
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
	case r.SourceID <= 0:
		return fmt.Errorf("store: forge history read of %s names no source id", r.Source.RepoKey)
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
	case r.Parent != (ForgeHistoryRef{}) && (r.Parent.RepoKey == "" || r.Parent.Namespace == "" || r.Parent.Repo == ""):
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
// ones missing and removes every other, with its pull requests. A source
// kept keeps its id and its rows. It reports whether it changed any.
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
	removed, err := tx.Exec(`DELETE FROM forge_history_sources WHERE repo_key NOT IN (SELECT w.value ->> 'repoKey' FROM json_each(?1) w)`, listed)
	if err != nil {
		return false, fmt.Errorf("%s: remove: %w", action, err)
	}
	added, err := tx.Exec(`INSERT INTO forge_history_sources(repo_key, forge, host, namespace, repo)
		SELECT w.value ->> 'repoKey', w.value ->> 'forge', w.value ->> 'host', w.value ->> 'namespace', w.value ->> 'repo'
		FROM json_each(?1) w WHERE true
		ON CONFLICT(repo_key) DO NOTHING`, listed)
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

const forgeHistorySourceColumns = `id, repo_key, forge, host, namespace, repo, own_head, parent_key, parent_namespace, parent_repo,
	walk, reach, owed, read_at, failure_kind, failure, failure_detail, failed_at`

func scanForgeHistorySource(row interface{ Scan(...any) error }) (ForgeHistorySource, error) {
	var src ForgeHistorySource
	var walk, detail string
	var reach sql.NullInt64
	err := row.Scan(&src.ID, &src.RepoKey, &src.Forge, &src.Host, &src.Namespace, &src.Repo, &src.OwnHead,
		&src.Parent.RepoKey, &src.Parent.Namespace, &src.Parent.Repo, &walk, &reach, &src.Owed, &src.ReadAt,
		&src.FailureKind, &src.Failure, &detail, &src.FailedAt)
	if src.Parent.RepoKey != "" {
		src.Parent.Forge, src.Parent.Host = src.Forge, src.Host
	}
	src.Walk, src.Reach, src.HasReach = []byte(walk), reach.Int64, reach.Valid
	if detail != "" {
		src.FailureDetail = []byte(detail)
	}
	return src, err
}

// ListForgeHistorySources reads every source, by repository key.
func (s *Store) ListForgeHistorySources() ([]ForgeHistorySource, error) {
	rows, err := s.reader().Query(`SELECT ` + forgeHistorySourceColumns + ` FROM forge_history_sources ORDER BY repo_key`)
	if err != nil {
		return nil, fmt.Errorf("store: list forge history sources: %w", err)
	}
	return scanForgeHistorySources(rows, "list forge history sources")
}

func scanForgeHistorySources(rows *sql.Rows, action string) ([]ForgeHistorySource, error) {
	var out []ForgeHistorySource
	for rows.Next() {
		src, err := scanForgeHistorySource(rows)
		if err != nil {
			return nil, errors.Join(fmt.Errorf("store: %s: %w", action, err), rows.Close())
		}
		out = append(out, src)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("store: %s: %w", action, err)
	}
	return out, nil
}

// GetForgeHistorySource reads source id; false when it is not stored.
func (s *Store) GetForgeHistorySource(id int64) (ForgeHistorySource, bool, error) {
	src, err := scanForgeHistorySource(s.reader().QueryRow(`SELECT `+forgeHistorySourceColumns+` FROM forge_history_sources WHERE id = ?`, id))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return ForgeHistorySource{}, false, nil
	case err != nil:
		return ForgeHistorySource{}, false, fmt.Errorf("store: read forge history source %d: %w", id, err)
	}
	return src, true, nil
}

// ApplyForgeHistory applies one read of a source in one transaction, and
// only while the source it began from is stored: its pull requests
// replace the stored ones of their numbers, with their keys and seen_at
// the read's time; each one's acts replace or add to its stored ones; the
// read's acts add to the stored pull requests'; the source's state and
// failure are recorded; and every pull request with no update and no act
// since the window's start is pruned. A re-read pull request with
// ActsWhole keeps only the acts it carries, so a withdrawn approval or a
// deleted comment leaves.
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
	err = tx.QueryRow(`SELECT id FROM forge_history_sources WHERE id = ? AND repo_key = ?`, read.SourceID, read.Source.RepoKey).Scan(&id)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("%s (source %d): %w", action, read.SourceID, ErrForgeHistoryGone)
	case err != nil:
		return fmt.Errorf("%s: read source: %w", action, err)
	}
	steps := []struct {
		what string
		sql  string
		args []any
	}{
		{"store pull requests", forgeHistoryPRsSQL, []any{id, prs, read.ReadAt}},
		{"clear keys", forgeHistoryClearKeysSQL, []any{id, prs}},
		{"store keys", forgeHistoryKeysSQL, []any{id, keysJSON}},
		{"clear acts", forgeHistoryClearActsSQL, []any{id, wholeJSON}},
	}
	for _, step := range steps {
		if _, err := tx.Exec(step.sql, step.args...); err != nil {
			return fmt.Errorf("%s: %s: %w", action, step.what, err)
		}
	}
	var orphan int64
	switch err := tx.QueryRow(forgeHistoryOrphanActSQL, id, actsJSON).Scan(&orphan); {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return fmt.Errorf("%s: check acts: %w", action, err)
	default:
		return fmt.Errorf("%s: an act names pull request %d, which neither the read nor the store holds", action, orphan)
	}
	for _, step := range []struct {
		what string
		sql  string
		args []any
	}{
		{"store acts", forgeHistoryActsSQL, []any{id, actsJSON}},
		{"prune", forgeHistoryPruneSQL, []any{id, read.WindowStart}},
	} {
		if _, err := tx.Exec(step.sql, step.args...); err != nil {
			return fmt.Errorf("%s: %s: %w", action, step.what, err)
		}
	}
	if read.OwnHead != "" {
		if _, err := tx.Exec(`UPDATE forge_history_sources SET own_head = ?2, parent_key = ?3, parent_namespace = ?4, parent_repo = ?5 WHERE id = ?1`,
			id, read.OwnHead, read.Parent.RepoKey, read.Parent.Namespace, read.Parent.Repo); err != nil {
			return fmt.Errorf("%s: record the repository: %w", action, err)
		}
	}
	if read.Walk != nil {
		reach := sql.NullInt64{Int64: read.Reach, Valid: !read.NoReach}
		if _, err := tx.Exec(`UPDATE forge_history_sources SET walk = ?2, reach = ?3, owed = ?4 WHERE id = ?1`,
			id, string(read.Walk), reach, read.Owed); err != nil {
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

// forgeHistoryPRsSQL stores a read's pull requests (?2) of source ?1,
// seen at ?3.
const forgeHistoryPRsSQL = `INSERT INTO forge_history_prs(source_id, number, url, title, head_ref, head_repo, state, author, author_name,
	created_at, updated_at, merged_at, closed_at, seen_at)
SELECT ?1, p.value ->> 'n', p.value ->> 'url', p.value ->> 'title', p.value ->> 'headRef', p.value ->> 'headRepo',
	p.value ->> 'state', p.value ->> 'author', p.value ->> 'authorName',
	p.value ->> 'created', p.value ->> 'updated', p.value ->> 'merged', p.value ->> 'closed', ?3
FROM json_each(?2) p WHERE true
ON CONFLICT(source_id, number) DO UPDATE SET url = excluded.url, title = excluded.title, head_ref = excluded.head_ref,
	head_repo = excluded.head_repo, state = excluded.state, author = excluded.author, author_name = excluded.author_name,
	created_at = excluded.created_at, updated_at = excluded.updated_at, merged_at = excluded.merged_at, closed_at = excluded.closed_at,
	seen_at = excluded.seen_at`

// The writer's other statements, each driven by its JSON list (?2) and
// reaching the stored rows of source ?1 by their keys: a read costs its
// own rows, whatever the source holds (TestForgeHistoryWriterPlans). The
// prune walks the source's update index below the window's start (?2).
// An act keeps a stored name when its read names nobody.
const (
	forgeHistoryClearKeysSQL = `DELETE FROM forge_history_keys WHERE source_id = CAST(?1 AS INTEGER) AND number IN (SELECT p.value ->> 'n' FROM json_each(?2) p)`
	forgeHistoryKeysSQL      = `INSERT INTO forge_history_keys(key, source_id, number)
SELECT DISTINCT k.value ->> 'k', ?1, k.value ->> 'n' FROM json_each(?2) k`
	forgeHistoryClearActsSQL = `DELETE FROM forge_history_acts WHERE source_id = CAST(?1 AS INTEGER) AND number IN (SELECT value FROM json_each(?2))`
	forgeHistoryOrphanActSQL = `SELECT a.value ->> 'n' FROM json_each(?2) a WHERE NOT EXISTS (
	SELECT 1 FROM forge_history_prs p WHERE p.source_id = CAST(?1 AS INTEGER) AND p.number = a.value ->> 'n') LIMIT 1`
	forgeHistoryActsSQL = `INSERT INTO forge_history_acts(source_id, number, kind, login, name, at)
SELECT ?1, a.value ->> 'n', a.value ->> 'kind', a.value ->> 'login', a.value ->> 'name', a.value ->> 'at' FROM json_each(?2) a WHERE true
ON CONFLICT(source_id, number, kind, login) DO UPDATE SET at = min(at, excluded.at), name = iif(excluded.name <> '', excluded.name, name)`
	forgeHistoryPruneSQL = `DELETE FROM forge_history_prs WHERE source_id = CAST(?1 AS INTEGER) AND updated_at < CAST(?2 AS INTEGER) AND NOT EXISTS (
	SELECT 1 FROM forge_history_acts a WHERE a.source_id = CAST(?1 AS INTEGER) AND a.number = forge_history_prs.number AND a.at > CAST(?2 AS INTEGER) - 1)`
)

// ForgeHistoryRow is a stored pull request that names a key, with its
// source's id and the wall time (Unix ms) the forge was last asked for
// it.
type ForgeHistoryRow struct {
	SourceID int64
	SeenAt   int64
	ForgeHistoryPR
}

// ForgeHistoryWork is what the stored history holds for a ticket of
// Work's origins, read in one snapshot: Sources are the origins' sources
// and their parents', Rows the pull requests that name the key and are
// Work's (ForgeHistoryForWork).
type ForgeHistoryWork struct {
	Sources []ForgeHistorySource
	Rows    []ForgeHistoryRow
}

// forgeHistoryWorkSourcesSQL reads the sources of origins ?1 and of the
// parents they name.
const forgeHistoryWorkSourcesSQL = `SELECT ` + forgeHistorySourceColumns + ` FROM forge_history_sources WHERE repo_key IN (
	SELECT value FROM json_each(?1)
	UNION SELECT o.parent_key FROM json_each(?1) w CROSS JOIN forge_history_sources o ON o.repo_key = w.value WHERE o.parent_key <> '')
ORDER BY repo_key`

// forgeHistoryWorkRowsSQL reads the pull requests that name key ?1 of
// the sources of origins ?2, and those of an origin's parent whose head
// is that origin.
const forgeHistoryWorkRowsSQL = `WITH o AS MATERIALIZED (
	SELECT s.id, s.own_head, s.parent_key FROM json_each(?2) w CROSS JOIN forge_history_sources s ON s.repo_key = w.value)
SELECT p.source_id, p.seen_at, p.number, p.url, p.title, p.head_ref, p.head_repo, p.state, p.author, p.author_name,
	p.created_at, p.updated_at, p.merged_at, p.closed_at
FROM forge_history_keys k CROSS JOIN forge_history_prs p ON p.source_id = k.source_id AND p.number = k.number
WHERE k.key = ?1 AND (k.source_id IN (SELECT id FROM o) OR EXISTS (
	SELECT 1 FROM o CROSS JOIN forge_history_sources par ON par.repo_key = o.parent_key
	WHERE o.own_head <> '' AND par.id = k.source_id AND p.head_repo = o.own_head))
ORDER BY p.source_id, p.number`

// ForgeHistoryForWork reads, in one snapshot, the sources of origins
// (repository keys) and of the parents they name, and the pull requests
// that name key and are Work's: every one of an origin's source, and one
// of an origin's parent whose head is that origin (its source's own
// head). A pull request of another repository, or of a parent with
// another head, is never Work's.
func (s *Store) ForgeHistoryForWork(key string, origins []string) (ForgeHistoryWork, error) {
	listed, err := jsonArray(origins)
	if err != nil {
		return ForgeHistoryWork{}, err
	}
	return mirrorReadSnapshot(s, "read the forge history of "+key, func(q sqlQueryer) (ForgeHistoryWork, error) {
		var out ForgeHistoryWork
		rows, err := q.Query(forgeHistoryWorkSourcesSQL, listed)
		if err != nil {
			return out, err
		}
		if out.Sources, err = scanForgeHistorySources(rows, "read the sources of "+key); err != nil {
			return out, err
		}
		err = scanMirrorRows(q, forgeHistoryWorkRowsSQL, []any{strings.ToUpper(key), listed}, func(row rowScanner) error {
			var r ForgeHistoryRow
			if err := row.Scan(&r.SourceID, &r.SeenAt, &r.Number, &r.URL, &r.Title, &r.HeadRef, &r.HeadRepo, &r.State, &r.Author, &r.AuthorName,
				&r.CreatedAt, &r.UpdatedAt, &r.MergedAt, &r.ClosedAt); err != nil {
				return err
			}
			out.Rows = append(out.Rows, r)
			return nil
		})
		return out, err
	})
}

// forgeHistoryKnownSQL answers which numbers of list ?2 source ?1 stores,
// each by its key.
const forgeHistoryKnownSQL = `SELECT p.number FROM json_each(?2) n
CROSS JOIN forge_history_prs p ON p.source_id = CAST(?1 AS INTEGER) AND p.number = n.value`

// KnownForgeHistoryPRs reports which of numbers source id stores, so a
// reader fetches only the pull requests an act names that it has never
// read.
func (s *Store) KnownForgeHistoryPRs(id int64, numbers []int64) (map[int64]bool, error) {
	listed, err := jsonArray(numbers)
	if err != nil {
		return nil, err
	}
	rows, err := s.reader().Query(forgeHistoryKnownSQL, id, listed)
	if err != nil {
		return nil, fmt.Errorf("store: read the known pull requests of source %d: %w", id, err)
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

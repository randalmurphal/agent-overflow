package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
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
	// proves one for every stream of the walk.
	Reach    int64
	HasReach bool
	// Owed reports that the walk has work left, which the next round
	// reads.
	Owed bool
	// ActsOwed reports that the last read left acts of a pull request it
	// read unread (the walk's follow-up pages): until the next read, the
	// stored acts of the pull requests inside Reach are not all of them.
	ActsOwed bool
	// ReadAt is the wall time (Unix ms) the last read landed, failed or
	// not; 0 before one.
	ReadAt int64
	// FailureKind, Failure and FailureDetail are the last read's
	// failure, its kind, the sentence a person reads and the app's JSON
	// detail of it, at FailedAt; empty when it landed.
	FailureKind, Failure string
	FailureDetail        []byte
	FailedAt             int64
	// GoodAt is the wall time (Unix ms) of the last read that landed
	// without a failure; 0 before one.
	GoodAt int64
	// Viewer is the login of the account whose token read the
	// repository; empty until a read says.
	Viewer string
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
// ms, 0 for one that has not happened. SeenAt is this computer's time when
// the request that read it started. Keys are the ticket keys its title
// and source branch name (trackerapp.TicketKeys).
type ForgeHistoryPR struct {
	Number     int64    `json:"n"`
	URL        string   `json:"url"`
	Title      string   `json:"title"`
	HeadRef    string   `json:"headRef"`
	HeadRepo   string   `json:"headRepo"`
	State      string   `json:"state"`
	Author     string   `json:"author"`
	AuthorName string   `json:"authorName"`
	CreatedAt  int64    `json:"created"`
	UpdatedAt  int64    `json:"updated"`
	MergedAt   int64    `json:"merged"`
	ClosedAt   int64    `json:"closed"`
	SeenAt     int64    `json:"seen"`
	Keys       []string `json:"-"`
}

// ForgeHistoryAct is one person's approval or comment on a pull request,
// at its earliest time (Unix ms). Acts are add-only: a stored act keeps
// the earliest time of any read, and its name when a read names nobody.
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
	// Viewer replaces the stored one when set.
	Viewer string
	PRs    []ForgeHistoryPR
	// Acts are acts of pull requests stored or in PRs, added to theirs.
	// One whose pull request neither holds fails the write: the reader
	// resolves every act's pull request before it moves past the act.
	Acts []ForgeHistoryAct
	// Walk, Reach, HasReach, Owed and ActsOwed replace the stored ones
	// when Walk is set: a read that read nothing leaves them. Without
	// HasReach the source has no reach (ForgeHistorySource.HasReach).
	Walk     []byte
	Reach    int64
	HasReach bool
	Owed     bool
	ActsOwed bool
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
		case pr.Number <= 0 || pr.UpdatedAt <= 0 || pr.SeenAt <= 0 || pr.CreatedAt < 0 || pr.MergedAt < 0 || pr.ClosedAt < 0:
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
	}
	for _, a := range r.Acts {
		if a.Number <= 0 || a.Kind != ForgeActApproved && a.Kind != ForgeActCommented || a.Login == "" || a.At <= 0 {
			return fmt.Errorf("store: forge history read of %s has an invalid act %+v", r.Source.RepoKey, a)
		}
	}
	return nil
}

// ForgeHistoryUse says that the credit of tracker connection ConnectionID
// reads the source of repository RepoKey: a project listing a board of
// the connection clones it.
type ForgeHistoryUse struct {
	ConnectionID string `json:"c"`
	RepoKey      string `json:"r"`
}

// forgeHistoryCreditSourcesSQL lists, by connection, the sources its
// credit reads: the ones it uses and the parent of each that is a fork.
const forgeHistoryCreditSourcesSQL = `SELECT u.connection_id, u.source_id FROM forge_history_uses u
UNION SELECT u.connection_id, par.id FROM forge_history_uses u
	CROSS JOIN forge_history_sources o ON o.id = u.source_id AND o.parent_key <> ''
	CROSS JOIN forge_history_sources par ON par.repo_key = o.parent_key`

// forgeHistoryCreditSources reads forgeHistoryCreditSourcesSQL as one
// string per connection and source.
func forgeHistoryCreditSources(q sqlQueryer) (map[string]map[int64]bool, error) {
	out := map[string]map[int64]bool{}
	err := scanMirrorRows(q, forgeHistoryCreditSourcesSQL, nil, func(r rowScanner) error {
		var conn string
		var id int64
		if err := r.Scan(&conn, &id); err != nil {
			return err
		}
		if out[conn] == nil {
			out[conn] = map[int64]bool{}
		}
		out[conn][id] = true
		return nil
	})
	return out, err
}

// SetForgeHistorySources makes the stored sources wanted and the stored
// uses uses, in one transaction: it adds the sources missing and removes
// every other, with its pull requests, and likewise the uses; a use of a
// connection or a repository that is not stored is not written. A source
// kept keeps its id and its rows. It reports whether it changed any
// source, and the connections whose credit reads other sources than
// before (forgeHistoryCreditSourcesSQL), sorted.
func (s *Store) SetForgeHistorySources(wanted []ForgeHistoryRef, uses []ForgeHistoryUse) (changed bool, connections []string, err error) {
	for _, r := range wanted {
		if err := r.check(); err != nil {
			return false, nil, err
		}
	}
	listed, err := jsonArray(wanted)
	if err != nil {
		return false, nil, fmt.Errorf("store: encode forge history sources: %w", err)
	}
	used, err := jsonArray(uses)
	if err != nil {
		return false, nil, fmt.Errorf("store: encode forge history uses: %w", err)
	}
	const action = "store: set forge history sources"
	tx, err := s.db.Begin()
	if err != nil {
		return false, nil, fmt.Errorf("%s: begin: %w", action, err)
	}
	defer rollbackTx(tx, action, &err)
	before, err := forgeHistoryCreditSources(tx)
	if err != nil {
		return false, nil, fmt.Errorf("%s: read the uses: %w", action, err)
	}
	removed, err := tx.Exec(`DELETE FROM forge_history_sources WHERE repo_key NOT IN (SELECT w.value ->> 'repoKey' FROM json_each(?1) w)`, listed)
	if err != nil {
		return false, nil, fmt.Errorf("%s: remove: %w", action, err)
	}
	added, err := tx.Exec(`INSERT INTO forge_history_sources(repo_key, forge, host, namespace, repo)
		SELECT w.value ->> 'repoKey', w.value ->> 'forge', w.value ->> 'host', w.value ->> 'namespace', w.value ->> 'repo'
		FROM json_each(?1) w WHERE true
		ON CONFLICT(repo_key) DO NOTHING`, listed)
	if err != nil {
		return false, nil, fmt.Errorf("%s: add: %w", action, err)
	}
	if _, err := tx.Exec(`WITH w AS MATERIALIZED (SELECT u.value ->> 'c' AS c, s.id FROM json_each(?1) u
			CROSS JOIN forge_history_sources s ON s.repo_key = u.value ->> 'r')
		DELETE FROM forge_history_uses WHERE (connection_id, source_id) NOT IN (SELECT c, id FROM w)`, used); err != nil {
		return false, nil, fmt.Errorf("%s: remove uses: %w", action, err)
	}
	if _, err := tx.Exec(`INSERT INTO forge_history_uses(connection_id, source_id)
		SELECT DISTINCT c.id, s.id FROM json_each(?1) u
		CROSS JOIN tracker_connections c ON c.id = u.value ->> 'c'
		CROSS JOIN forge_history_sources s ON s.repo_key = u.value ->> 'r'
		WHERE true ON CONFLICT(connection_id, source_id) DO NOTHING`, used); err != nil {
		return false, nil, fmt.Errorf("%s: add uses: %w", action, err)
	}
	after, err := forgeHistoryCreditSources(tx)
	if err != nil {
		return false, nil, fmt.Errorf("%s: read the uses written: %w", action, err)
	}
	if err := tx.Commit(); err != nil {
		return false, nil, fmt.Errorf("%s: commit: %w", action, err)
	}
	gone, err := removed.RowsAffected()
	if err != nil {
		return false, nil, err
	}
	made, err := added.RowsAffected()
	if err != nil {
		return false, nil, err
	}
	for conn := range before {
		if !maps.Equal(before[conn], after[conn]) {
			connections = append(connections, conn)
		}
	}
	for conn := range after {
		if before[conn] == nil {
			connections = append(connections, conn)
		}
	}
	slices.Sort(connections)
	return gone+made > 0, connections, nil
}

// ListForgeHistoryUses reads every use, by connection and repository.
func (s *Store) ListForgeHistoryUses() ([]ForgeHistoryUse, error) {
	var out []ForgeHistoryUse
	err := scanMirrorRows(s.reader(), `SELECT u.connection_id, s.repo_key FROM forge_history_uses u
		CROSS JOIN forge_history_sources s ON s.id = u.source_id ORDER BY u.connection_id, s.repo_key`, nil, func(r rowScanner) error {
		var u ForgeHistoryUse
		err := r.Scan(&u.ConnectionID, &u.RepoKey)
		out = append(out, u)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("store: list forge history uses: %w", err)
	}
	return out, nil
}

// ForgeHistoryUsers are the connections whose credit reads source id: the
// ones that use it, and the ones that use a fork of it.
func (s *Store) ForgeHistoryUsers(id int64) ([]string, error) {
	var out []string
	err := scanMirrorRows(s.reader(), `SELECT u.connection_id FROM forge_history_uses u WHERE u.source_id = CAST(?1 AS INTEGER)
		UNION SELECT u.connection_id FROM forge_history_sources par
		CROSS JOIN forge_history_sources o ON o.parent_key = par.repo_key
		CROSS JOIN forge_history_uses u ON u.source_id = o.id
		WHERE par.id = CAST(?1 AS INTEGER) ORDER BY 1`, []any{id}, func(r rowScanner) error {
		var conn string
		err := r.Scan(&conn)
		out = append(out, conn)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("store: read the connections that use forge history source %d: %w", id, err)
	}
	return out, nil
}

const forgeHistorySourceColumns = `id, repo_key, forge, host, namespace, repo, own_head, parent_key, parent_namespace, parent_repo,
	walk, reach, owed, read_at, failure_kind, failure, failure_detail, failed_at, acts_owed, good_at, viewer`

func scanForgeHistorySource(row interface{ Scan(...any) error }) (ForgeHistorySource, error) {
	var src ForgeHistorySource
	var walk, detail string
	var reach sql.NullInt64
	err := row.Scan(&src.ID, &src.RepoKey, &src.Forge, &src.Host, &src.Namespace, &src.Repo, &src.OwnHead,
		&src.Parent.RepoKey, &src.Parent.Namespace, &src.Parent.Repo, &walk, &reach, &src.Owed, &src.ReadAt,
		&src.FailureKind, &src.Failure, &detail, &src.FailedAt, &src.ActsOwed, &src.GoodAt, &src.Viewer)
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

// ForgeHistoryChange is what an applied read changed of what credit
// counts from its source (tracker_cache_credit_forge.go): Keys are the
// ticket keys whose pull requests' acts are other than before (an act
// added, moved earlier or named, a key or a head repository of a pull
// request with acts changed, a pull request with acts pruned), sorted;
// State reports that what the source covers or says of its failure is:
// its reach, whether acts are owed, its failure, or its own head or
// parent. A read that changes neither moves no credit.
type ForgeHistoryChange struct {
	Keys  []string
	State bool
}

// Empty reports that the read moved no credit.
func (c ForgeHistoryChange) Empty() bool { return len(c.Keys) == 0 && !c.State }

// ApplyForgeHistory applies one read of a source in one transaction, and
// only while the source it began from is stored: its pull requests
// replace the stored ones of their numbers, with their keys and seen_at;
// its acts add to the stored ones, never removing one (ForgeHistoryAct);
// the source's state and failure are recorded; and every pull request
// with no update and no act since the window's start is pruned. It
// answers what the read changed of what credit counts.
func (s *Store) ApplyForgeHistory(read ForgeHistoryRead) (_ ForgeHistoryChange, err error) {
	if err := read.check(); err != nil {
		return ForgeHistoryChange{}, err
	}
	prs, err := jsonArray(read.PRs)
	if err != nil {
		return ForgeHistoryChange{}, fmt.Errorf("store: encode forge history pull requests: %w", err)
	}
	type keyRow struct {
		Key    string `json:"k"`
		Number int64  `json:"n"`
	}
	var keys []keyRow
	for _, pr := range read.PRs {
		for _, key := range pr.Keys {
			keys = append(keys, keyRow{key, pr.Number})
		}
	}
	keysJSON, err := jsonArray(keys)
	if err != nil {
		return ForgeHistoryChange{}, err
	}
	actsJSON, err := jsonArray(read.Acts)
	if err != nil {
		return ForgeHistoryChange{}, err
	}
	action := "store: apply forge history read of " + read.Source.RepoKey
	tx, err := s.db.Begin()
	if err != nil {
		return ForgeHistoryChange{}, fmt.Errorf("%s: begin: %w", action, err)
	}
	defer rollbackTx(tx, action, &err)
	// was is the state credit reads of the source before the read.
	type state struct {
		reach                             sql.NullInt64
		actsOwed                          bool
		kind, failure, ownHead, parentKey string
	}
	var id int64
	var was state
	err = tx.QueryRow(`SELECT id, reach, acts_owed, failure_kind, failure, own_head, parent_key FROM forge_history_sources WHERE id = ? AND repo_key = ?`,
		read.SourceID, read.Source.RepoKey).Scan(&id, &was.reach, &was.actsOwed, &was.kind, &was.failure, &was.ownHead, &was.parentKey)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return ForgeHistoryChange{}, fmt.Errorf("%s (source %d): %w", action, read.SourceID, ErrForgeHistoryGone)
	case err != nil:
		return ForgeHistoryChange{}, fmt.Errorf("%s: read source: %w", action, err)
	}
	// moved are the keys of pull requests whose acts credit reads changed,
	// and maybe the keys and numbers a key or head change touched, which
	// count only for a pull request that has acts.
	moved := map[string]bool{}
	var maybe []keyRow
	keyOf := func(r rowScanner) error {
		var key string
		err := r.Scan(&key)
		moved[key] = true
		return err
	}
	pairOf := func(r rowScanner) error {
		var k keyRow
		err := r.Scan(&k.Key, &k.Number)
		maybe = append(maybe, k)
		return err
	}
	// touched are the numbers whose acts the read added to or changed.
	var touched []int64
	numberOf := func(r rowScanner) error {
		var n int64
		err := r.Scan(&n)
		touched = append(touched, n)
		return err
	}
	for _, step := range []struct {
		what string
		sql  string
		args []any
		scan func(rowScanner) error
	}{
		{"read the heads that changed", forgeHistoryHeadChangesSQL, []any{id, prs}, pairOf},
		{"store pull requests", forgeHistoryPRsSQL, []any{id, prs}, nil},
		{"clear keys", forgeHistoryClearKeysSQL, []any{id, prs, keysJSON}, pairOf},
		{"store keys", forgeHistoryKeysSQL, []any{id, keysJSON}, pairOf},
	} {
		if err := execForgeHistory(tx, step.sql, step.args, step.scan); err != nil {
			return ForgeHistoryChange{}, fmt.Errorf("%s: %s: %w", action, step.what, err)
		}
	}
	var orphan int64
	switch err := tx.QueryRow(forgeHistoryOrphanActSQL, id, actsJSON).Scan(&orphan); {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return ForgeHistoryChange{}, fmt.Errorf("%s: check acts: %w", action, err)
	default:
		return ForgeHistoryChange{}, fmt.Errorf("%s: an act names pull request %d, which neither the read nor the store holds", action, orphan)
	}
	if err := execForgeHistory(tx, forgeHistoryActsSQL, []any{id, actsJSON}, numberOf); err != nil {
		return ForgeHistoryChange{}, fmt.Errorf("%s: store acts: %w", action, err)
	}
	touchedJSON, err := jsonArray(touched)
	if err != nil {
		return ForgeHistoryChange{}, err
	}
	maybeJSON, err := jsonArray(maybe)
	if err != nil {
		return ForgeHistoryChange{}, err
	}
	for _, step := range []struct {
		what string
		sql  string
		args []any
		scan func(rowScanner) error
	}{
		{"read the keys of the acts", forgeHistoryActKeysSQL, []any{id, touchedJSON}, keyOf},
		{"read the keys that moved acts", forgeHistoryMovedKeysSQL, []any{id, maybeJSON}, keyOf},
		{"read the keys the prune moves", forgeHistoryPrunedKeysSQL, []any{id, read.WindowStart}, keyOf},
		{"prune", forgeHistoryPruneSQL, []any{id, read.WindowStart}, nil},
	} {
		if err := execForgeHistory(tx, step.sql, step.args, step.scan); err != nil {
			return ForgeHistoryChange{}, fmt.Errorf("%s: %s: %w", action, step.what, err)
		}
	}
	now := was
	if read.OwnHead != "" {
		now.ownHead, now.parentKey = read.OwnHead, read.Parent.RepoKey
		if _, err := tx.Exec(`UPDATE forge_history_sources SET own_head = ?2, parent_key = ?3, parent_namespace = ?4, parent_repo = ?5 WHERE id = ?1`,
			id, read.OwnHead, read.Parent.RepoKey, read.Parent.Namespace, read.Parent.Repo); err != nil {
			return ForgeHistoryChange{}, fmt.Errorf("%s: record the repository: %w", action, err)
		}
	}
	if read.Walk != nil {
		now.reach, now.actsOwed = sql.NullInt64{Int64: read.Reach, Valid: read.HasReach}, read.ActsOwed
		if !read.HasReach {
			now.reach.Int64 = 0
		}
		if _, err := tx.Exec(`UPDATE forge_history_sources SET walk = ?2, reach = ?3, owed = ?4, acts_owed = ?5 WHERE id = ?1`,
			id, string(read.Walk), now.reach, read.Owed, read.ActsOwed); err != nil {
			return ForgeHistoryChange{}, fmt.Errorf("%s: record the walk: %w", action, err)
		}
	}
	failedAt := int64(0)
	if read.Failure != "" {
		failedAt = read.ReadAt
	}
	now.kind, now.failure = read.FailureKind, read.Failure
	if _, err := tx.Exec(`UPDATE forge_history_sources SET read_at = ?2, failure_kind = ?3, failure = ?4, failure_detail = ?5, failed_at = ?6,
		good_at = iif(?4 = '', ?2, good_at), viewer = iif(?7 <> '', ?7, viewer) WHERE id = ?1`,
		id, read.ReadAt, read.FailureKind, read.Failure, string(read.FailureDetail), failedAt, read.Viewer); err != nil {
		return ForgeHistoryChange{}, fmt.Errorf("%s: record the read: %w", action, err)
	}
	if err := tx.Commit(); err != nil {
		return ForgeHistoryChange{}, fmt.Errorf("%s: commit: %w", action, err)
	}
	return ForgeHistoryChange{Keys: slices.Sorted(maps.Keys(moved)), State: now != was}, nil
}

// execForgeHistory runs one of the writer's statements, handing each row
// it returns to scan; nil for a statement that returns none.
func execForgeHistory(tx *sql.Tx, query string, args []any, scan func(rowScanner) error) error {
	if scan == nil {
		_, err := tx.Exec(query, args...)
		return err
	}
	return scanMirrorRows(tx, query, args, scan)
}

// forgeHistoryPRsSQL stores a read's pull requests (?2) of source ?1.
const forgeHistoryPRsSQL = `INSERT INTO forge_history_prs(source_id, number, url, title, head_ref, head_repo, state, author, author_name,
	created_at, updated_at, merged_at, closed_at, seen_at)
SELECT ?1, p.value ->> 'n', p.value ->> 'url', p.value ->> 'title', p.value ->> 'headRef', p.value ->> 'headRepo',
	p.value ->> 'state', p.value ->> 'author', p.value ->> 'authorName',
	p.value ->> 'created', p.value ->> 'updated', p.value ->> 'merged', p.value ->> 'closed', p.value ->> 'seen'
FROM json_each(?2) p WHERE true
ON CONFLICT(source_id, number) DO UPDATE SET url = excluded.url, title = excluded.title, head_ref = excluded.head_ref,
	head_repo = excluded.head_repo, state = excluded.state, author = excluded.author, author_name = excluded.author_name,
	created_at = excluded.created_at, updated_at = excluded.updated_at, merged_at = excluded.merged_at, closed_at = excluded.closed_at,
	seen_at = excluded.seen_at`

// The writer's other statements, each driven by its JSON list and
// reaching the stored rows of source ?1 by their keys: a read costs its
// own rows, whatever the source holds (TestForgeHistoryWriterPlans). The
// prune walks the source's update index below the window's start (?2).
// An act keeps a stored name when its read names nobody. The ones that
// return rows say what the read changed (ForgeHistoryChange):
//   - head changes: the keys and numbers of the read's pull requests (?2)
//     stored with another head repository, read before they are written;
//   - clear keys: the stored keys of the read's pull requests (?2) the
//     read does not name (?3), removed; store keys: the ones it names
//     (?2) that are not stored, added;
//   - acts: the numbers of the acts (?2) added, moved earlier or named;
//   - act keys: the keys of the numbers ?2; moved keys: the keys of the
//     list ?2 whose pull request has an act; pruned keys: the keys of the
//     pull requests with acts the prune removes.
const (
	forgeHistoryHeadChangesSQL = `SELECT k.key, k.number FROM json_each(?2) p
CROSS JOIN forge_history_prs s ON s.source_id = CAST(?1 AS INTEGER) AND s.number = p.value ->> 'n'
CROSS JOIN forge_history_keys k ON k.source_id = s.source_id AND k.number = s.number
WHERE s.head_repo IS NOT p.value ->> 'headRepo'`
	forgeHistoryClearKeysSQL = `DELETE FROM forge_history_keys WHERE source_id = CAST(?1 AS INTEGER) AND number IN (SELECT p.value ->> 'n' FROM json_each(?2) p)
	AND (key, number) NOT IN (SELECT k.value ->> 'k', CAST(k.value ->> 'n' AS INTEGER) FROM json_each(?3) k)
RETURNING key, number`
	forgeHistoryKeysSQL = `INSERT INTO forge_history_keys(key, source_id, number)
SELECT DISTINCT k.value ->> 'k', ?1, k.value ->> 'n' FROM json_each(?2) k WHERE true
ON CONFLICT(key, source_id, number) DO NOTHING
RETURNING key, number`
	forgeHistoryOrphanActSQL = `SELECT a.value ->> 'n' FROM json_each(?2) a WHERE NOT EXISTS (
	SELECT 1 FROM forge_history_prs p WHERE p.source_id = CAST(?1 AS INTEGER) AND p.number = a.value ->> 'n') LIMIT 1`
	forgeHistoryActsSQL = `INSERT INTO forge_history_acts(source_id, number, kind, login, name, at)
SELECT ?1, a.value ->> 'n', a.value ->> 'kind', a.value ->> 'login', a.value ->> 'name', a.value ->> 'at' FROM json_each(?2) a WHERE true
ON CONFLICT(source_id, number, kind, login) DO UPDATE SET at = min(at, excluded.at), name = iif(excluded.name <> '', excluded.name, name)
	WHERE excluded.at < at OR excluded.name <> '' AND excluded.name IS NOT name
RETURNING number`
	forgeHistoryActKeysSQL = `SELECT DISTINCT k.key FROM json_each(?2) n
CROSS JOIN forge_history_keys k ON k.source_id = CAST(?1 AS INTEGER) AND k.number = n.value`
	forgeHistoryMovedKeysSQL = `SELECT DISTINCT m.value ->> 'k' FROM json_each(?2) m WHERE EXISTS (
	SELECT 1 FROM forge_history_acts a WHERE a.source_id = CAST(?1 AS INTEGER) AND a.number = m.value ->> 'n')`
	forgeHistoryPrunedSQL = `forge_history_prs.source_id = CAST(?1 AS INTEGER) AND forge_history_prs.updated_at < CAST(?2 AS INTEGER) AND NOT EXISTS (
	SELECT 1 FROM forge_history_acts a WHERE a.source_id = CAST(?1 AS INTEGER) AND a.number = forge_history_prs.number AND a.at > CAST(?2 AS INTEGER) - 1)`
	forgeHistoryPrunedKeysSQL = `SELECT DISTINCT k.key FROM forge_history_prs
CROSS JOIN forge_history_keys k ON k.source_id = forge_history_prs.source_id AND k.number = forge_history_prs.number
WHERE ` + forgeHistoryPrunedSQL + ` AND EXISTS (
	SELECT 1 FROM forge_history_acts a WHERE a.source_id = forge_history_prs.source_id AND a.number = forge_history_prs.number)`
	forgeHistoryPruneSQL = `DELETE FROM forge_history_prs WHERE ` + forgeHistoryPrunedSQL
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
// and their parents', Rows the pull requests of them that are Work's and
// name the key or are ones Work asked for by number (ForgeHistoryForWork).
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

// forgeHistoryWorkRowsSQL reads the pull requests that name key ?1, or
// that list ?3 names by repository and number, of the sources of origins
// ?2, and those of an origin's parent whose head is that origin.
const forgeHistoryWorkRowsSQL = `WITH o AS MATERIALIZED (
	SELECT s.id, s.own_head, s.parent_key FROM json_each(?2) w CROSS JOIN forge_history_sources s ON s.repo_key = w.value),
c AS MATERIALIZED (
	SELECT k.source_id, k.number FROM forge_history_keys k WHERE k.key = ?1
	UNION SELECT s.id, x.value ->> 'n' FROM json_each(?3) x CROSS JOIN forge_history_sources s ON s.repo_key = x.value ->> 'r')
SELECT p.source_id, p.seen_at, p.number, p.url, p.title, p.head_ref, p.head_repo, p.state, p.author, p.author_name,
	p.created_at, p.updated_at, p.merged_at, p.closed_at
FROM c CROSS JOIN forge_history_prs p ON p.source_id = c.source_id AND p.number = c.number
WHERE c.source_id IN (SELECT id FROM o) OR EXISTS (
	SELECT 1 FROM o CROSS JOIN forge_history_sources par ON par.repo_key = o.parent_key
	WHERE o.own_head <> '' AND par.id = c.source_id AND p.head_repo = o.own_head)
ORDER BY p.source_id, p.number`

// ForgeHistoryNumber names a pull request of a repository (its key,
// git.RepoReference.Identity).
type ForgeHistoryNumber struct {
	RepoKey string `json:"r"`
	Number  int64  `json:"n"`
}

// ForgeHistoryForWork reads, in one snapshot, the sources of origins
// (repository keys) and of the parents they name, and the pull requests
// of them that are Work's and name key or are named by numbers: every
// one of an origin's source, and one of an origin's parent whose head is
// that origin (its source's own head). A pull request of another
// repository, or of a parent with another head, is never Work's.
func (s *Store) ForgeHistoryForWork(key string, origins []string, numbers []ForgeHistoryNumber) (ForgeHistoryWork, error) {
	listed, err := jsonArray(origins)
	if err != nil {
		return ForgeHistoryWork{}, err
	}
	named, err := jsonArray(numbers)
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
		err = scanMirrorRows(q, forgeHistoryWorkRowsSQL, []any{strings.ToUpper(key), listed, named}, func(row rowScanner) error {
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

// ForgeHistorySourcesForWork reads the sources of origins (repository
// keys) and of the parents they name, as ForgeHistoryForWork does,
// without their rows.
func (s *Store) ForgeHistorySourcesForWork(origins []string) ([]ForgeHistorySource, error) {
	listed, err := jsonArray(origins)
	if err != nil {
		return nil, err
	}
	rows, err := s.reader().Query(forgeHistoryWorkSourcesSQL, listed)
	if err != nil {
		return nil, fmt.Errorf("store: read the forge history sources of %d origins: %w", len(origins), err)
	}
	return scanForgeHistorySources(rows, "read the forge history sources of Work's origins")
}

// StoredForgeHistoryPR is what a source stores of a pull request: its
// update time and the time a read last saw it (Unix ms).
type StoredForgeHistoryPR struct {
	UpdatedAt, SeenAt int64
}

// forgeHistoryStoredSQL answers the numbers of list ?2 source ?1 stores,
// each by its key, with their update and seen times.
const forgeHistoryStoredSQL = `SELECT p.number, p.updated_at, p.seen_at FROM json_each(?2) n
CROSS JOIN forge_history_prs p ON p.source_id = CAST(?1 AS INTEGER) AND p.number = n.value`

// StoredForgeHistoryPRs answers which of numbers source id stores, with
// what it stores of each, so a reader reads again only what changed and
// fetches only the pull requests an act names that it has never read.
func (s *Store) StoredForgeHistoryPRs(id int64, numbers []int64) (map[int64]StoredForgeHistoryPR, error) {
	listed, err := jsonArray(numbers)
	if err != nil {
		return nil, err
	}
	rows, err := s.reader().Query(forgeHistoryStoredSQL, id, listed)
	if err != nil {
		return nil, fmt.Errorf("store: read the stored pull requests of source %d: %w", id, err)
	}
	out := map[int64]StoredForgeHistoryPR{}
	for rows.Next() {
		var n int64
		var pr StoredForgeHistoryPR
		if err := rows.Scan(&n, &pr.UpdatedAt, &pr.SeenAt); err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		out[n] = pr
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

package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// The two thread reads the agent thread tools resolve and list with. Both
// answer in SQL what a caller would otherwise answer by loading every
// thread row and filtering in Go: this computer can hold tens of thousands
// of threads, and `threadColumns` carries correlated subqueries, so a Go
// filter over the whole listing pays for a proposed-plan probe and a turn
// aggregate on every row it then throws away.

// ResolveThreadPrefix returns the threads whose id starts with prefix,
// ordered by id, at most limit rows. Every mode is included, hidden ones
// too, and so are archived threads: a reference names a thread the caller
// already knows about, and deciding which of those a particular caller may
// see belongs to the caller's own visibility rule, not to the lookup.
// Threads this computer gave away are excluded, as `owned_threads`
// excludes them everywhere else.
//
// The match is a half-open range over the id primary key rather than a
// LIKE. `threads.id` collates BINARY and `case_sensitive_like` is off, so
// `id LIKE 'abcd%'` would both scan the table and match case-insensitively,
// which is not what a thread reference means; a range bound is exact,
// case-sensitive, index-driven, and gives `_`, `%` and `\` no special
// meaning, so there is no escaping to get wrong.
//
// An empty prefix matches nothing. A prefix-less listing is
// ListThreadsByActivity, and answering it here would hand back the whole
// table under the name of a lookup. A non-positive limit matches nothing
// either.
func (s *Store) ResolveThreadPrefix(prefix string, limit int) ([]Thread, error) {
	if prefix == "" || limit <= 0 {
		return nil, nil
	}
	conditions := []string{"threads.id >= ?"}
	args := []any{prefix}
	if upper, bounded := prefixUpperBound(prefix); bounded {
		conditions = append(conditions, "threads.id < ?")
		args = append(args, upper)
	}
	rows, err := s.reader().Query(
		`SELECT `+threadColumns+` FROM owned_threads AS threads
		  WHERE `+strings.Join(conditions, " AND ")+`
		  ORDER BY threads.id ASC
		  LIMIT `+strconv.Itoa(limit),
		args...,
	)
	if err != nil {
		return nil, fmt.Errorf("store: resolve thread prefix %q: %w", prefix, err)
	}
	defer rows.Close()

	var threads []Thread
	for rows.Next() {
		thread, err := scanThread(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan thread prefix row: %w", err)
		}
		threads = append(threads, thread)
	}
	return threads, rows.Err()
}

// prefixUpperBound returns the exclusive upper bound of the byte range a
// prefix owns: the prefix with its last byte below 0xFF incremented.
// bounded is false when every byte is 0xFF, which has no upper bound and
// leaves the range open above.
//
// SQLite compares BINARY text by bytes, so incrementing a byte is the
// correct bound whether or not the result is well-formed UTF-8.
func prefixUpperBound(prefix string) (string, bool) {
	bytes := []byte(prefix)
	for i := len(bytes) - 1; i >= 0; i-- {
		if bytes[i] == 0xFF {
			continue
		}
		bytes[i]++
		return string(bytes[:i+1]), true
	}
	return "", false
}

// ListThreadsByActivity is the query-less half of thread search: this
// computer's threads by last activity, newest first, under the same filter
// SearchThreads applies to its hits. Last activity is the newest completed
// turn, falling back to updated_at, which is the clock the sidebar and the
// tools both call a thread's activity; `updated_at` alone would order a
// thread by the last row written to it, including a write no reader sees.
//
// Ties break on id so LIMIT/OFFSET paging is stable across calls. Kinds and
// SnippetBudget are ignored; every other filter field applies.
func (s *Store) ListThreadsByActivity(filter ThreadSearchFilter) ([]Thread, error) {
	conditions, args := filter.threadRowConditions("threads.")
	if len(filter.ThreadIDs) > 0 {
		clause, clauseArgs := inClause("threads.id", filter.ThreadIDs)
		conditions = append(conditions, clause)
		args = append(args, clauseArgs...)
	}
	limit := filter.Limit
	if limit < 1 {
		limit = 20
	}
	offset := filter.Offset
	if offset < 0 {
		offset = 0
	}
	args = append(args, offset)

	rows, err := s.reader().Query(
		`SELECT `+threadColumns+` FROM owned_threads AS threads
		  WHERE `+strings.Join(conditions, " AND ")+`
		  ORDER BY `+threadLastActivityExpr("threads.")+` DESC, threads.id ASC
		  LIMIT `+strconv.Itoa(limit)+` OFFSET ?`,
		args...,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list threads by activity: %w", err)
	}
	defer rows.Close()

	var threads []Thread
	for rows.Next() {
		thread, err := scanThread(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan thread activity row: %w", err)
		}
		threads = append(threads, thread)
	}
	return threads, rows.Err()
}

// ListOwnedThreadsByID reads the threads this computer owns by id, in one
// statement. The ranked search answers in ITEM rows, and resolving each
// hit's thread on its own would pay the correlated subqueries of
// `threadColumns` once per hit rather than once per page.
//
// An id this computer does not own is simply absent, so the caller matches
// the result by id rather than by position.
func (s *Store) ListOwnedThreadsByID(ids []string) ([]Thread, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	clause, args := inClause("threads.id", ids)
	rows, err := s.reader().Query(
		`SELECT `+threadColumns+` FROM owned_threads AS threads WHERE `+clause, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list threads by id: %w", err)
	}
	defer rows.Close()

	var threads []Thread
	for rows.Next() {
		thread, err := scanThread(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan thread by id: %w", err)
		}
		threads = append(threads, thread)
	}
	return threads, rows.Err()
}

// ThreadLastMessage is the newest message of one thread's latest turn: a
// top-level assistant text or a reader-authored user text, with the tail of
// its text. It answers "who spoke last" for a listing without a transcript
// read.
type ThreadLastMessage struct {
	// Kind is user_text or assistant_text.
	Kind string
	// At is the row's last write in Unix milliseconds.
	At int64
	// Tail is the last threadLastMessageTailRunes runes of the text, and
	// Clipped says the text was longer.
	Tail    string
	Clipped bool
}

// threadLastMessageTailRunes bounds the text a listing carries per thread.
// The end of a message is the part that says whether it asked something.
const threadLastMessageTailRunes = 400

// threadLastMessageSQL is one statement for a whole page of threads. Each
// thread's read is pinned to its latest turn row, so the arms walk one
// turn's rows on the local indexes and only the chunk references whose turn
// range holds that turn, whatever the thread's history holds. A thread whose
// latest turn has no message yet, or that has no turn row, has none.
var threadLastMessageSQL = func() string {
	arms, _ := correlatedTimelineArms(timelineSelection{
		Columns: func(string, string) string {
			return `items.kind AS kind, items.updated_at AS at, items.summary AS summary,
			        items.turn_index AS turn_index, items.item_index AS item_index`
		},
		Thread:  "page.id",
		Turn:    "page.turn",
		Where:   mainTimelineFilterFor("items.") + ` AND (items.kind = 'assistant_text' OR (` + userMessageTickFilterFor("items.") + `))`,
		OrderBy: "turn_index DESC, item_index DESC",
		Limit:   1,
	})
	return `WITH page(id, turn) AS MATERIALIZED (
		  SELECT ids.value, (SELECT MAX(turn_index) FROM turns WHERE turns.thread_id = ids.value)
		    FROM json_each(?) AS ids
		)
		SELECT page.id,
		       (SELECT json_object('kind', last.kind, 'at', last.at,
		                           'tail', substr(last.summary, -` + strconv.Itoa(threadLastMessageTailRunes) + `),
		                           'clipped', length(last.summary) > ` + strconv.Itoa(threadLastMessageTailRunes) + `)
		          FROM (` + arms + `) AS last)
		  FROM page`
}()

// ListThreadLastMessages reads the latest-turn message of each thread in one
// statement, keyed by thread id. A thread with none is absent.
func (s *Store) ListThreadLastMessages(threadIDs []string) (map[string]ThreadLastMessage, error) {
	out := make(map[string]ThreadLastMessage, len(threadIDs))
	if len(threadIDs) == 0 {
		return out, nil
	}
	ids, err := json.Marshal(threadIDs)
	if err != nil {
		return nil, fmt.Errorf("store: encode thread ids for last messages: %w", err)
	}
	rows, err := s.reader().Query(threadLastMessageSQL, string(ids))
	if err != nil {
		return nil, fmt.Errorf("store: list thread last messages: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var encoded sql.NullString
		if err := rows.Scan(&id, &encoded); err != nil {
			return nil, fmt.Errorf("store: scan thread last message: %w", err)
		}
		if !encoded.Valid {
			continue
		}
		var row struct {
			Kind    string `json:"kind"`
			At      int64  `json:"at"`
			Tail    string `json:"tail"`
			Clipped int    `json:"clipped"`
		}
		if err := json.Unmarshal([]byte(encoded.String), &row); err != nil {
			return nil, fmt.Errorf("store: decode thread last message of %s: %w", id, err)
		}
		out[id] = ThreadLastMessage{Kind: row.Kind, At: row.At, Tail: row.Tail, Clipped: row.Clipped != 0}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate thread last messages: %w", err)
	}
	return out, nil
}

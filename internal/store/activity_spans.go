package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// The activity the recorder measured (migration v174): spans of the
// person's input and the days each thread started a turn. Only
// internal/activity writes spans and prunes both tables; InsertTurn writes
// the thread days.

// MaxActivitySpanMs is the longest span the recorder writes: it closes a
// span this long and opens the next, so a read of a range finds every
// span that reaches it among those that start at most this long before.
const MaxActivitySpanMs = 24 * 60 * 60 * 1000

// jiraOnSQL is whether a Jira connection is on: the time source the
// recorded activity serves. While none is, nothing is recorded.
const jiraOnSQL = `EXISTS (SELECT 1 FROM tracker_connections WHERE kind = 'jira' AND enabled = 1)`

// ActivitySpan is a span of the person's input: ID 0 until it is written.
type ActivitySpan struct {
	ID       int64
	ThreadID string
	Phone    bool
	StartMs  int64
	EndMs    int64
}

var insertActivitySpanSQL = `INSERT INTO activity_spans(thread_id, phone, start_ms, end_ms)
 SELECT ?1, ?2, ?3, ?4 WHERE ` + jiraOnSQL + ` RETURNING id`

// SaveActivitySpans writes spans in one transaction: it inserts each with
// ID 0, while a Jira connection is on, and moves the end of each with an
// ID. It returns each span's ID in spans' order, 0 for one not inserted
// because no Jira connection is on.
func (s *Store) SaveActivitySpans(spans []ActivitySpan) ([]int64, error) {
	ids := make([]int64, len(spans))
	if len(spans) == 0 {
		return ids, nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("store: save activity spans: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for i, sp := range spans {
		if sp.EndMs < sp.StartMs || sp.EndMs-sp.StartMs > MaxActivitySpanMs {
			return nil, fmt.Errorf("store: save activity spans: span %d to %d is not 0 to %d ms long", sp.StartMs, sp.EndMs, MaxActivitySpanMs)
		}
		if sp.ID != 0 {
			if _, err := tx.Exec(`UPDATE activity_spans SET end_ms = ?2 WHERE id = ?1`, sp.ID, sp.EndMs); err != nil {
				return nil, fmt.Errorf("store: save activity span %d: %w", sp.ID, err)
			}
			ids[i] = sp.ID
			continue
		}
		err := tx.QueryRow(insertActivitySpanSQL, sp.ThreadID, boolToInt(sp.Phone), sp.StartMs, sp.EndMs).Scan(&ids[i])
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("store: save activity span: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store: save activity spans: %w", err)
	}
	return ids, nil
}

// ActivityRow is a span of a read, with the Jira ticket its thread is
// linked to now (none for a thread with no Jira link, or none at all).
type ActivityRow struct {
	ThreadID string
	Phone    bool
	StartMs  int64
	EndMs    int64
	Site     string
	IssueID  string
	IssueKey string
}

// activitySpansSQL reads the spans that reach ?1 to ?2 (Unix ms), by
// start: a span starts at most MaxActivitySpanMs before what it reaches,
// so the start index bounds the read on both sides.
var activitySpansSQL = `SELECT s.thread_id, s.phone, s.start_ms, s.end_ms, coalesce(t.site, ''), coalesce(t.issue_id, ''), coalesce(t.issue_key, '')
 FROM activity_spans s LEFT JOIN thread_tickets t ON t.thread_id = s.thread_id AND t.kind = 'jira'
 WHERE s.start_ms >= ?1 - ` + strconv.Itoa(MaxActivitySpanMs) + ` AND s.start_ms < ?2 AND s.end_ms > ?1
 ORDER BY s.start_ms, s.id`

// ActivitySpans are the spans that reach fromMs to toMs, by start.
func (s *Store) ActivitySpans(fromMs, toMs int64) ([]ActivityRow, error) {
	rows, err := s.reader().Query(activitySpansSQL, fromMs, toMs)
	if err != nil {
		return nil, fmt.Errorf("store: activity spans: %w", err)
	}
	var out []ActivityRow
	for rows.Next() {
		var r ActivityRow
		var phone int
		if err := rows.Scan(&r.ThreadID, &phone, &r.StartMs, &r.EndMs, &r.Site, &r.IssueID, &r.IssueKey); err != nil {
			return nil, errors.Join(fmt.Errorf("store: scan activity span: %w", err), rows.Close())
		}
		r.Phone = phone == 1
		out = append(out, r)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("store: activity spans: %w", err)
	}
	return out, nil
}

// ThreadDay is a thread that started a turn on Day, with the Jira ticket
// it is linked to now (none when it has no Jira link).
type ThreadDay struct {
	Day      string
	ThreadID string
	Site     string
	IssueID  string
	IssueKey string
}

var threadDaysSQL = `SELECT d.day, d.thread_id, coalesce(t.site, ''), coalesce(t.issue_id, ''), coalesce(t.issue_key, '')
 FROM thread_days d LEFT JOIN thread_tickets t ON t.thread_id = d.thread_id AND t.kind = 'jira'
 WHERE d.day >= ?1 AND d.day <= ?2 ORDER BY d.day, d.thread_id`

// ThreadDays are the threads that started a turn from day from to day to,
// both YYYY-MM-DD and included, by day.
func (s *Store) ThreadDays(from, to string) ([]ThreadDay, error) {
	rows, err := s.reader().Query(threadDaysSQL, from, to)
	if err != nil {
		return nil, fmt.Errorf("store: thread days: %w", err)
	}
	var out []ThreadDay
	for rows.Next() {
		var d ThreadDay
		if err := rows.Scan(&d.Day, &d.ThreadID, &d.Site, &d.IssueID, &d.IssueKey); err != nil {
			return nil, errors.Join(fmt.Errorf("store: scan thread day: %w", err), rows.Close())
		}
		out = append(out, d)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("store: thread days: %w", err)
	}
	return out, nil
}

// PruneActivity deletes the spans that start before beforeMs and the
// thread days before beforeDay (YYYY-MM-DD), and nothing else, in one
// transaction. It returns how many rows of each it deleted.
func (s *Store) PruneActivity(beforeMs int64, beforeDay string) (spans, days int64, err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, 0, fmt.Errorf("store: prune activity: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.Exec(`DELETE FROM activity_spans WHERE start_ms < ?1`, beforeMs)
	if err == nil {
		spans, err = res.RowsAffected()
	}
	if err == nil {
		res, err = tx.Exec(`DELETE FROM thread_days WHERE day < ?1`, beforeDay)
	}
	if err == nil {
		days, err = res.RowsAffected()
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		return 0, 0, fmt.Errorf("store: prune activity: %w", err)
	}
	return spans, days, nil
}

// localDay is the local date of a Unix millisecond time, as the thread
// days and the time draft name days.
func localDay(ms int64) string { return time.UnixMilli(ms).Format(time.DateOnly) }

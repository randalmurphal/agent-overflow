package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// Temporary measurement (not committed): the list query with the link
// column against the same query with NULL in its place, in one process.
func BenchmarkListThreadsLinkColumnAB(b *testing.B) {
	base := filepath.Join(b.TempDir(), "base.sqlite")
	seedThreadListBench(b, base)
	query, args := listThreadsWithItemsQuery()
	if !strings.Contains(query, threadTicketLinkColumn) {
		b.Fatal("the list query has no link column")
	}
	without := strings.Replace(query, threadTicketLinkColumn, "NULL", 1)
	plainColumn := `(SELECT json_object(
        'kind', tt.kind, 'site', tt.site, 'issueId', tt.issue_id, 'key', tt.issue_key, 'title', tt.issue_title,
        'statusId', tt.status_id, 'statusName', tt.status_name, 'statusGroup', tt.status_group,
        'origin', tt.origin, 'linkedAt', tt.linked_at) FROM thread_tickets tt WHERE tt.thread_id = threads.id)`
	plain := strings.Replace(query, threadTicketLinkColumn, plainColumn, 1)
	nodecode := strings.Replace(query, threadTicketLinkColumn, `CASE WHEN `+threadTicketLinkColumn+` IS NULL THEN NULL ELSE '{}' END`, 1)
	for _, linked := range []int{0, 500} {
		path := filepath.Join(b.TempDir(), fmt.Sprintf("store-%d.sqlite", linked))
		if err := copyStoreFile(base, path); err != nil {
			b.Fatal(err)
		}
		s := openTestStore(b, path)
		linkThreadListBench(b, s, linked)
		for _, v := range []struct {
			name, q string
		}{{"with", query}, {"null", without}, {"plain", plain}, {"nodecode", nodecode}} {
			b.Run(fmt.Sprintf("linked=%d/link=%s", linked, v.name), func(b *testing.B) {
				for b.Loop() {
					rows, err := s.reader().Query(v.q, args...)
					if err != nil {
						b.Fatal(err)
					}
					n := 0
					for rows.Next() {
						if _, err := scanThread(rows); err != nil {
							b.Fatal(err)
						}
						n++
					}
					if err := rows.Close(); err != nil || n != threadListBenchThreads {
						b.Fatalf("listed %d, %v", n, err)
					}
				}
			})
		}
	}
}

// Temporary measurement: the link column alone, decoded as scanThread
// decodes it, over the same 5,000 threads.
func BenchmarkThreadLinkColumnAlone(b *testing.B) {
	base := filepath.Join(b.TempDir(), "base.sqlite")
	seedThreadListBench(b, base)
	for _, linked := range []int{0, 500} {
		path := filepath.Join(b.TempDir(), fmt.Sprintf("store-%d.sqlite", linked))
		if err := copyStoreFile(base, path); err != nil {
			b.Fatal(err)
		}
		s := openTestStore(b, path)
		linkThreadListBench(b, s, linked)
		for _, v := range []struct{ name, col string }{{"column", threadTicketLinkColumn}, {"null", "NULL"}} {
			q := `SELECT ` + v.col + ` FROM threads`
			b.Run(fmt.Sprintf("linked=%d/link=%s", linked, v.name), func(b *testing.B) {
				for b.Loop() {
					rows, err := s.reader().Query(q)
					if err != nil {
						b.Fatal(err)
					}
					n, got := 0, 0
					for rows.Next() {
						var raw sql.NullString
						if err := rows.Scan(&raw); err != nil {
							b.Fatal(err)
						}
						if raw.Valid {
							var link ThreadTicketLink
							if err := json.Unmarshal([]byte(raw.String), &link); err != nil {
								b.Fatal(err)
							}
							got++
						}
						n++
					}
					if err := rows.Close(); err != nil || n != threadListBenchThreads || (v.name == "column" && got != linked) {
						b.Fatalf("read %d rows, %d links, %v", n, got, err)
					}
				}
			})
		}
	}
}

// Temporary measurement: the list query with and without the link column,
// alternated inside each iteration so drift falls on both alike.
func BenchmarkListThreadsInterleaved(b *testing.B) {
	base := filepath.Join(b.TempDir(), "base.sqlite")
	seedThreadListBench(b, base)
	query, args := listThreadsWithItemsQuery()
	without := strings.Replace(query, threadTicketLinkColumn, "NULL", 1)
	for _, linked := range []int{0, 500} {
		path := filepath.Join(b.TempDir(), fmt.Sprintf("store-%d.sqlite", linked))
		if err := copyStoreFile(base, path); err != nil {
			b.Fatal(err)
		}
		s := openTestStore(b, path)
		linkThreadListBench(b, s, linked)
		read := func(q string) time.Duration {
			start := time.Now()
			rows, err := s.reader().Query(q, args...)
			if err != nil {
				b.Fatal(err)
			}
			for rows.Next() {
				if _, err := scanThread(rows); err != nil {
					b.Fatal(err)
				}
			}
			if err := rows.Close(); err != nil {
				b.Fatal(err)
			}
			return time.Since(start)
		}
		b.Run(fmt.Sprintf("linked=%d", linked), func(b *testing.B) {
			var with, null time.Duration
			i := 0
			for b.Loop() {
				if i%2 == 0 {
					with += read(query)
					null += read(without)
				} else {
					null += read(without)
					with += read(query)
				}
				i++
			}
			b.ReportMetric(float64(with.Microseconds())/float64(i), "with-µs/op")
			b.ReportMetric(float64(null.Microseconds())/float64(i), "null-µs/op")
			b.ReportMetric(100*(float64(with)/float64(null)-1), "added-%")
		})
	}
}

// threadLinksOnceSQL is the alternative: every link with its derived
// state, read once per list and joined to the rows in Go.
const threadLinksOnceSQL = `SELECT tt.thread_id, tt.kind, tt.site, tt.issue_id, tt.issue_key, tt.issue_title,
	tt.status_id, tt.status_name, tt.status_group, tt.origin, tt.linked_at,
	CASE WHEN seen.connection_id IS NOT NULL THEN iif(tt.missing, 'not_found', 'current')
	  ELSE CASE (SELECT MAX(c.enabled) FROM tracker_connections c
	              WHERE c.kind = tt.kind AND c.site = tt.site AND c.parent_id IS NULL)
	    WHEN 1 THEN 'not_checked' WHEN 0 THEN 'turned_off' ELSE 'no_connection' END END,
	COALESCE(seen.connection_id, (SELECT c.id FROM tracker_connections c
	  WHERE c.kind = tt.kind AND c.site = tt.site AND c.parent_id IS NULL AND c.enabled = 1 ORDER BY c.id LIMIT 1), '')
	FROM thread_tickets tt LEFT JOIN tracker_cache_roots seen ON seen.id = tt.seen_root`

// Temporary measurement: the list with the link column, against the list
// with NULL and the links read once and joined in Go, alternated.
func BenchmarkListThreadsJoinedInGo(b *testing.B) {
	base := filepath.Join(b.TempDir(), "base.sqlite")
	seedThreadListBench(b, base)
	query, args := listThreadsWithItemsQuery()
	without := strings.Replace(query, threadTicketLinkColumn, "NULL", 1)
	for _, linked := range []int{0, 500} {
		path := filepath.Join(b.TempDir(), fmt.Sprintf("store-%d.sqlite", linked))
		if err := copyStoreFile(base, path); err != nil {
			b.Fatal(err)
		}
		s := openTestStore(b, path)
		linkThreadListBench(b, s, linked)
		list := func(q string) []Thread {
			rows, err := s.reader().Query(q, args...)
			if err != nil {
				b.Fatal(err)
			}
			var out []Thread
			for rows.Next() {
				t, err := scanThread(rows)
				if err != nil {
					b.Fatal(err)
				}
				out = append(out, t)
			}
			if err := rows.Close(); err != nil {
				b.Fatal(err)
			}
			return out
		}
		joined := func() []Thread {
			threads := list(without)
			rows, err := s.reader().Query(threadLinksOnceSQL)
			if err != nil {
				b.Fatal(err)
			}
			links := map[string]*ThreadTicketLink{}
			for rows.Next() {
				var id string
				l := new(ThreadTicketLink)
				if err := rows.Scan(&id, &l.Kind, &l.Site, &l.IssueID, &l.Key, &l.Title, &l.StatusID, &l.StatusName, &l.StatusGroup,
					&l.Origin, &l.LinkedAt, &l.State, &l.ConnectionID); err != nil {
					b.Fatal(err)
				}
				links[id] = l
			}
			if err := rows.Close(); err != nil {
				b.Fatal(err)
			}
			got := 0
			for i := range threads {
				if l := links[threads[i].ID]; l != nil {
					threads[i].TicketLink = l
					got++
				}
			}
			if got != linked {
				b.Fatalf("joined %d links, want %d", got, linked)
			}
			return threads
		}
		b.Run(fmt.Sprintf("linked=%d", linked), func(b *testing.B) {
			var with, null, inGo time.Duration
			i := 0
			for b.Loop() {
				order := []int{0, 1, 2}
				order = append(order[i%3:], order[:i%3]...)
				for _, k := range order {
					start := time.Now()
					switch k {
					case 0:
						list(query)
						with += time.Since(start)
					case 1:
						list(without)
						null += time.Since(start)
					case 2:
						joined()
						inGo += time.Since(start)
					}
				}
				i++
			}
			b.ReportMetric(float64(null.Microseconds())/float64(i), "null-µs/op")
			b.ReportMetric(100*(float64(with)/float64(null)-1), "column-added-%")
			b.ReportMetric(100*(float64(inGo)/float64(null)-1), "go-join-added-%")
		})
	}
}

// Temporary: the opcodes of the guarded link column.
func TestExplainLinkGuardTmp(t *testing.T) {
	s := newTestStore(t)
	dump := func(q string, around bool) {
		rows, err := s.db.Query(`EXPLAIN ` + q)
		if err != nil {
			t.Fatal(err)
		}
		type op struct {
			addr       int
			code       string
			p1, p2, p3 int
			p4         sql.NullString
			p5         int
			comment    sql.NullString
		}
		var ops []op
		for rows.Next() {
			var o op
			if err := rows.Scan(&o.addr, &o.code, &o.p1, &o.p2, &o.p3, &o.p4, &o.p5, &o.comment); err != nil {
				t.Fatal(err)
			}
			ops = append(ops, o)
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
		for _, o := range ops {
			t.Logf("%4d %-14s %4d %4d %4d %s", o.addr, o.code, o.p1, o.p2, o.p3, o.p4.String)
		}
	}
	dump(`SELECT `+threadTicketLinkColumn+` FROM threads`, false)
	full, fullArgs := listThreadsWithItemsQuery()
	var pages []int
	prow, err := s.db.Query(`SELECT rootpage FROM sqlite_schema WHERE tbl_name = 'thread_tickets'`)
	if err != nil {
		t.Fatal(err)
	}
	for prow.Next() {
		var p int
		if err := prow.Scan(&p); err != nil {
			t.Fatal(err)
		}
		pages = append(pages, p)
	}
	if err := prow.Close(); err != nil {
		t.Fatal(err)
	}
	t.Logf("thread_tickets pages %v", pages)
	rows, err := s.db.Query(`EXPLAIN `+full, fullArgs...)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	var hits []int
	for rows.Next() {
		var addr, p1, p2, p3, p5 int
		var code string
		var p4, comment sql.NullString
		if err := rows.Scan(&addr, &code, &p1, &p2, &p3, &p4, &p5, &comment); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, fmt.Sprintf("%5d %-12s %5d %5d %5d %s", addr, code, p1, p2, p3, p4.String))
		if code == "OpenRead" && slices.Contains(pages, p2) {
			hits = append(hits, addr)
		}
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	for _, h := range hits {
		for i := max(0, h-6); i <= h+1 && i < len(lines); i++ {
			t.Logf("full %s", lines[i])
		}
		t.Logf("full ---")
	}
	plan, err := s.db.Query(`EXPLAIN QUERY PLAN SELECT ` + threadTicketLinkColumn + ` FROM threads`)
	if err != nil {
		t.Fatal(err)
	}
	for plan.Next() {
		var id, parent, unused int
		var detail string
		if err := plan.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		t.Logf("plan %d %d %s", id, parent, detail)
	}
	if err := plan.Close(); err != nil {
		t.Fatal(err)
	}
}

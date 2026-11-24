package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The mirror's benchmarks run against a store file holding a realistic
// mirror: one root, five boards of 1,000 issues each with 100 sprints a
// board, ten changes an issue, 2,000 worklogs, and 50 issues only a
// thread's link keeps. Each reports the statements one call runs
// (stmts/op).
//
//	make go-test GO_TEST_PKGS=./internal/store GO_TEST_FLAGS="-run '^$' -bench Mirror -benchmem"

// A 100-row page: all new, identical to what is stored from the same read
// or from a later one (each row's read time moves), or with 10 rows changed.
func BenchmarkMirrorWritePage(b *testing.B) {
	b.Run("new", func(b *testing.B) {
		s, root := openBenchMirror(b)
		measureMirror(b, func(i int) error {
			rows := make([]MirrorRow, 100)
			for r := range rows {
				rows[r] = benchRow(0, benchBoardIssues*benchBoards+i*100+r, 1000)
			}
			got, err := s.WriteMirrorRowPage(root, currentPage(s, root, benchPage(0, rows)), []byte("cursor"))
			return benchApplied(got, err, 100, 100)
		})
	})
	b.Run("identical", func(b *testing.B) {
		s, root := openBenchMirror(b)
		rows := make([]MirrorRow, 100)
		for r := range rows {
			rows[r] = benchRow(0, r, 1000)
		}
		measureMirror(b, func(int) error {
			got, err := s.WriteMirrorRowPage(root, currentPage(s, root, benchPage(0, rows)), []byte("cursor"))
			return benchApplied(got, err, 0, 0)
		})
	})
	b.Run("identical-later", func(b *testing.B) {
		s, root := openBenchMirror(b)
		rows := make([]MirrorRow, 100)
		for r := range rows {
			rows[r] = benchRow(0, r, 1000)
		}
		measureMirror(b, func(i int) error {
			page := benchPage(0, rows)
			page.ReadAt = mirrorReadAt(int64(2 + i))
			got, err := s.WriteMirrorRowPage(root, currentPage(s, root, page), []byte("cursor"))
			return benchApplied(got, err, 0, 0)
		})
	})
	b.Run("10-changed", func(b *testing.B) {
		s, root := openBenchMirror(b)
		rows := make([]MirrorRow, 100)
		for r := range rows {
			rows[r] = benchRow(0, r, 1000)
		}
		measureMirror(b, func(i int) error {
			for r := range 10 {
				rows[r*10].Card.UpdatedAt = int64(2000 + i)
			}
			got, err := s.WriteMirrorRowPage(root, currentPage(s, root, benchPage(0, rows)), []byte("cursor"))
			return benchApplied(got, err, 10, 0)
		})
	})
}

// A complete reconcile of a 1,000-issue board: nothing to do, and 50 rows
// unlisted, which the read by ID removes (put back between runs, untimed).
func BenchmarkMirrorReconcile(b *testing.B) {
	listing := make([]string, benchBoardIssues)
	for i := range listing {
		listing[i] = benchIssueID(0, i)
	}
	b.Run("nothing", func(b *testing.B) {
		s, root := openBenchMirror(b)
		measureMirror(b, func(int) error {
			_, err := s.ReconcileMirrorBoard(root, benchBoardID(0), currentLifetime(s, root, benchBoardID(0)), listing, true, mirrorReadAt(1))
			return err
		})
	})
	b.Run("50-unlisted", func(b *testing.B) {
		s, root := openBenchMirror(b)
		back := make([]MirrorRow, 50)
		for r := range back {
			back[r] = benchRow(0, r, 1000)
		}
		measureMirror(b, func(i int) error {
			got, err := s.ReconcileMirrorBoard(root, benchBoardID(0), currentLifetime(s, root, benchBoardID(0)), listing[50:], true, mirrorReadAt(int64(10+3*i)))
			if err != nil || len(got.Unsettled) != 50 {
				return fmt.Errorf("unsettled %d, %v", len(got.Unsettled), err)
			}
			gone, err := s.WriteMirrorRows(root, currentPage(s, root, MirrorRowPage{BoardID: benchBoardID(0), Missing: got.Unsettled, ReadAt: mirrorReadAt(int64(11 + 3*i))}))
			if err != nil || len(gone.Removed) != 50 {
				return fmt.Errorf("the read by ID removed %d, %v", len(gone.Removed), err)
			}
			b.StopTimer()
			page := benchPage(0, back)
			page.ReadAt = mirrorReadAt(int64(12 + 3*i))
			put, err := s.WriteMirrorRows(root, currentPage(s, root, page))
			b.StartTimer()
			return benchApplied(put, err, 0, 50)
		})
	})
}

// Histories of 20 changes replacing ones of 20 with other times: one
// issue, and a read batch of mirrorHistoryBatch.
func BenchmarkMirrorReplaceHistory(b *testing.B) {
	for _, n := range []int{1, mirrorHistoryBatch} {
		b.Run(fmt.Sprint(n, "-issues"), func(b *testing.B) {
			s, root := openBenchMirror(b)
			batch, sweep := make([]MirrorHistory, n), currentSweep(s, root)
			for k := range batch {
				batch[k].IssueID = benchIssueID(0, k)
			}
			revs := make([]int64, n)
			for k, h := range dueHistories(s, root, batch) {
				revs[k] = h.BoardsRev
			}
			measureMirror(b, func(i int) error {
				for k := range batch {
					batch[k] = benchHistory(benchIssueID(0, k), int64(2000+i), 20)
					batch[k].BoardsRev = revs[k]
				}
				results, _, err := s.ReplaceMirrorHistories(root, sweep, batch, MirrorCatalog{}, mirrorReadOf(root, int64(1_000_000+i)))
				if err != nil {
					return err
				}
				for _, r := range results {
					if !r.Changed || r.Superseded || r.Unkept {
						return fmt.Errorf("history of %s not applied: %+v", r.IssueID, r)
					}
				}
				return nil
			})
		})
	}
}

// A page of the issues due a history read, each with its boards, with
// every issue of the fixture due.
func BenchmarkMirrorHistoryDue(b *testing.B) {
	s, root := openBenchMirror(b)
	if _, err := s.db.Exec(`UPDATE tracker_cache_issues SET history_for = 0`); err != nil {
		b.Fatal(err)
	}
	measureMirror(b, func(int) error {
		_, _, err := s.ListMirrorHistoryDue(root, testNow)
		return err
	})
}

// The first page of 100 rows of each section kind of a 1,000-row board.
func BenchmarkMirrorSectionPage(b *testing.B) {
	for name, section := range map[string]MirrorSection{
		"backlog": {Kind: MirrorSectionBacklog},
		"sprint":  {Kind: MirrorSectionSprint, SprintID: strconv.Itoa(benchBoardSprints - 1)},
		"column":  {Kind: MirrorSectionColumn, Column: 0},
	} {
		b.Run(name, func(b *testing.B) {
			s, root := openBenchMirror(b)
			measureMirror(b, func(int) error {
				_, err := s.ListMirrorSection(root, benchBoardID(0), section, MirrorRowKey{})
				return err
			})
		})
	}
}

// The section totals with estimate sums of a 1,000-row board.
func BenchmarkMirrorCountSections(b *testing.B) {
	s, root := openBenchMirror(b)
	measureMirror(b, func(int) error {
		_, err := s.CountMirrorSections(root, benchBoardID(0))
		return err
	})
}

// The cleanup with nothing pending, the fixture's 50 linked-only issues
// included, and one root's sweep with nothing unkept.
func BenchmarkMirrorCleanupIdle(b *testing.B) {
	b.Run("finish", func(b *testing.B) {
		s, _ := openBenchMirror(b)
		measureMirror(b, func(int) error { return s.FinishMirrorCleanup(context.Background(), nil) })
	})
	b.Run("sweep", func(b *testing.B) {
		s, root := openBenchMirror(b)
		measureMirror(b, func(int) error { return s.sweepMirrorRoot(root, noMirrorPause) })
	})
}

// The listing check every board write runs, alone and as a whole empty
// board write.
func BenchmarkMirrorListingCheck(b *testing.B) {
	b.Run("statement", func(b *testing.B) {
		s, root := openBenchMirror(b)
		lifetime := currentLifetime(s, root, benchBoardID(0))
		measureMirror(b, func(int) error {
			var live, listed, retired, marked bool
			var readAt, removedAt, resetAt, pendingAt int64
			return s.db.QueryRow(mirrorBoardWriteSQL, root, benchBoardID(0), lifetime).Scan(&live, &listed, &retired, &readAt, &removedAt,
				&resetAt, &pendingAt, &marked)
		})
	})
	b.Run("write", func(b *testing.B) {
		s, root := openBenchMirror(b)
		measureMirror(b, func(i int) error {
			_, changed, err := s.RecordMirrorSyncAttempt(root, benchBoardID(0), currentAttempt(s, root, benchBoardID(0), MirrorSyncAttempt{ReadAt: mirrorReadAt(int64(10 + i))}))
			if err == nil && !changed {
				err = errors.New("the attempt was not recorded")
			}
			return err
		})
	})
}

// A read of the catalog's statuses with a later read time: identical (each
// status's read time moves), and with status 3 moved into the done group
// and back, which advances the revision of the five boards with a row in
// it after a walk of the root's 5,050 issues.
func BenchmarkMirrorStatusRead(b *testing.B) {
	catalog := func(group int) MirrorCatalog {
		st := benchStructure(0).Catalog
		st.Users = nil
		st.Statuses[2].Group = group
		return st
	}
	for name, groups := range map[string][]int{"identical": {MirrorGroupInProgress}, "done-flip": {MirrorGroupDone, MirrorGroupInProgress}} {
		b.Run(name, func(b *testing.B) {
			s, root := openBenchMirror(b)
			sweep := currentSweep(s, root)
			measureMirror(b, func(i int) error {
				got, err := s.UpsertMirrorCards(root, sweep, nil, catalog(groups[i%len(groups)]), mirrorReadAt(int64(10+i)))
				if err == nil && got.Catalog != (len(groups) > 1) {
					err = fmt.Errorf("catalog changed %v", got.Catalog)
				}
				return err
			})
		})
	}
}

// A board's lifetime: listed (its row's insert, whose foreign key probe
// walks the root's board rows), unlisted, and drained with no rows.
func BenchmarkMirrorBoardLifetime(b *testing.B) {
	s, _ := openBenchMirror(b)
	jira := benchConnectionID(b, s)
	listing := []ProjectTrackerBoard{}
	for board := range benchBoards {
		listing = append(listing, ProjectTrackerBoard{ConnectionID: jira, BoardID: benchBoardID(board)})
	}
	measureMirror(b, func(int) error {
		if _, err := s.ReplaceProjectTrackerBoards(defaultTestProjectID, append(slices.Clone(listing), ProjectTrackerBoard{ConnectionID: jira, BoardID: "99"})); err != nil {
			return err
		}
		if _, err := s.ReplaceProjectTrackerBoards(defaultTestProjectID, listing); err != nil {
			return err
		}
		return s.FinishMirrorCleanup(context.Background(), nil)
	})
}

// The fixture's storage: bytes per card and per change, table and index
// sizes (dbstat), and the file. Run with -benchtime=1x.
func BenchmarkMirrorStorage(b *testing.B) {
	s, _, path := openBenchMirrorFile(b)
	sizes := map[string]float64{}
	err := scanMirrorRows(s.db, `SELECT name, sum(pgsize) FROM dbstat WHERE name LIKE '%tracker_cache%' GROUP BY name`, nil, func(r rowScanner) error {
		var name string
		var size float64
		err := r.Scan(&name, &size)
		sizes[name] = size
		return err
	})
	if err != nil {
		b.Fatal(err)
	}
	counts := countMirrorRows(b, s.db)
	for range b.N {
	}
	issues := sizes["sqlite_autoindex_tracker_cache_issues_1"] + sizes["tracker_cache_issues"]
	b.ReportMetric(sizes["tracker_cache_issues"]/float64(counts["tracker_cache_issues"]), "B/card")
	b.ReportMetric(sizes["tracker_cache_changes"]/float64(counts["tracker_cache_changes"]), "B/change")
	b.ReportMetric(sizes["tracker_cache_board_issues"]/float64(counts["tracker_cache_board_issues"]), "B/row")
	var names []string
	for name := range sizes {
		names = append(names, name)
	}
	slices.Sort(names)
	var total float64
	var report strings.Builder
	for _, name := range names {
		total += sizes[name]
		fmt.Fprintf(&report, "%s %.0f bytes, %d rows\n", name, sizes[name], counts[name])
	}
	info, err := os.Stat(path)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportMetric(total, "mirror-bytes")
	b.ReportMetric(float64(info.Size()), "file-bytes")
	b.Logf("issues with their key index: %.0f bytes\n%s", issues, report.String())
}

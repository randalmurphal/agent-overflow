package store

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestForgeHistoryAtScale measures, for each count of pull requests
// AO_TRACKER_SCALE names, the forge history's writer on one source:
// storing them a read of 500 at a time (GitHub's 10 pages of 50), each
// with 2 keys, 2 approvals and 2 comments; a steady read of one pull
// request beside them all; the reader's lookup of a page's 50 stored
// pull requests; Work's read of a key and 20 listed numbers; and the
// prune of them all once
// the window has passed them. Skipped without AO_TRACKER_SCALE.
func TestForgeHistoryAtScale(t *testing.T) {
	env := os.Getenv("AO_TRACKER_SCALE")
	if env == "" {
		t.Skip("AO_TRACKER_SCALE names no sizes")
	}
	for field := range strings.SplitSeq(env, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(field))
		if err != nil || n < 500 {
			t.Fatalf("AO_TRACKER_SCALE=%q: each size is a whole number of at least 500", env)
		}
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			s := newTestStore(t)
			if _, _, err := s.SetForgeHistorySources([]ForgeHistoryRef{testOrigin}, nil); err != nil {
				t.Fatal(err)
			}
			id := sourceID(t, s, testOrigin)
			const window = int64(1_000_000)
			pr := func(i int64, updated int64) ForgeHistoryPR {
				return testPR(i, fmt.Sprintf("WEB-%d Change %d", i, i), fmt.Sprintf("web-%d", i+1), updated, fmt.Sprintf("WEB-%d", i), fmt.Sprintf("WEB-%d", i+1))
			}
			acts := func(i int64, updated int64) []ForgeHistoryAct {
				var out []ForgeHistoryAct
				for k, login := range []string{"bob", "carol", "dave", "erin"} {
					out = append(out, ForgeHistoryAct{Number: i, Kind: ForgeActApproved + k%2, Login: login, Name: login, At: updated - int64(k)})
				}
				return out
			}
			var slowest time.Duration
			start := time.Now()
			for from := int64(1); from <= int64(n); from += 500 {
				read := ForgeHistoryRead{SourceID: id, Source: testOrigin, ReadAt: 9_000_000, WindowStart: window, Walk: []byte(`{}`)}
				for i := from; i < from+500 && i <= int64(n); i++ {
					read.PRs = append(read.PRs, pr(i, window+i*10))
					read.Acts = append(read.Acts, acts(i, window+i*10)...)
				}
				one := time.Now()
				if _, err := s.ApplyForgeHistory(read); err != nil {
					t.Fatal(err)
				}
				slowest = max(slowest, time.Since(one))
			}
			t.Logf("%d pull requests: stored in %d reads of 500 in %v, the slowest %v", n, (n+499)/500, time.Since(start).Round(time.Millisecond), slowest.Round(time.Millisecond))

			start = time.Now()
			applyForge(t, s, ForgeHistoryRead{Source: testOrigin, ReadAt: 9_000_001, WindowStart: window, Walk: []byte(`{}`),
				PRs: []ForgeHistoryPR{pr(int64(n/2), window+int64(n)*10+1)}, Acts: acts(int64(n/2), window+int64(n)*10+1)})
			t.Logf("%d pull requests: a steady read of one in %v", n, time.Since(start).Round(time.Microsecond))

			page := make([]int64, 0, 50)
			for i := range int64(50) {
				page = append(page, int64(n)-i)
			}
			start = time.Now()
			if stored, err := s.StoredForgeHistoryPRs(id, page); err != nil || len(stored) != 50 {
				t.Fatalf("stored = %d, %v", len(stored), err)
			}
			t.Logf("%d pull requests: a page's lookup of 50 in %v", n, time.Since(start).Round(time.Microsecond))

			var numbers []ForgeHistoryNumber
			for i := range int64(20) {
				numbers = append(numbers, ForgeHistoryNumber{RepoKey: testOrigin.RepoKey, Number: int64(n)/5 + i})
			}
			start = time.Now()
			work, err := s.ForgeHistoryForWork(fmt.Sprintf("WEB-%d", n/3), []string{testOrigin.RepoKey}, numbers)
			if err != nil || len(work.Rows) != 22 {
				t.Fatalf("by key and number = %d rows, %v", len(work.Rows), err)
			}
			t.Logf("%d pull requests: a key's read with 20 numbers in %v", n, time.Since(start).Round(time.Microsecond))

			start = time.Now()
			applyForge(t, s, ForgeHistoryRead{Source: testOrigin, ReadAt: 9_000_002, WindowStart: window + int64(n)*10 + 2, Walk: []byte(`{}`)})
			t.Logf("%d pull requests: the prune of them all in %v", n, time.Since(start).Round(time.Millisecond))
			assertRows(t, s.db, `SELECT count(*) FROM forge_history_prs`, "0")
			assertRows(t, s.db, `SELECT (SELECT count(*) FROM forge_history_acts) + (SELECT count(*) FROM forge_history_keys)`, "0")
		})
	}
}

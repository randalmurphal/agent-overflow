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
// request beside them all; a key's read; and the prune of them all once
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
			if _, err := s.SetForgeHistorySources([]ForgeHistoryRef{testOrigin}); err != nil {
				t.Fatal(err)
			}
			const window = int64(1_000_000)
			pr := func(i int64, updated int64) ForgeHistoryPR {
				p := testPR(i, fmt.Sprintf("WEB-%d Change %d", i, i), fmt.Sprintf("web-%d", i+1), updated, fmt.Sprintf("WEB-%d", i), fmt.Sprintf("WEB-%d", i+1))
				p.ActsWhole = true
				for k, login := range []string{"bob", "carol", "dave", "erin"} {
					p.Acts = append(p.Acts, ForgeHistoryAct{Number: i, Kind: ForgeActApproved + k%2, Login: login, Name: login, At: updated - int64(k)})
				}
				return p
			}
			var slowest time.Duration
			start := time.Now()
			for from := int64(1); from <= int64(n); from += 500 {
				read := ForgeHistoryRead{Source: testOrigin, ReadAt: 9_000_000, WindowStart: window, Walk: []byte(`{}`)}
				for i := from; i < from+500 && i <= int64(n); i++ {
					read.PRs = append(read.PRs, pr(i, window+i*10))
				}
				one := time.Now()
				if err := s.ApplyForgeHistory(read); err != nil {
					t.Fatal(err)
				}
				slowest = max(slowest, time.Since(one))
			}
			t.Logf("%d pull requests: stored in %d reads of 500 in %v, the slowest %v", n, (n+499)/500, time.Since(start).Round(time.Millisecond), slowest.Round(time.Millisecond))

			start = time.Now()
			applyForge(t, s, ForgeHistoryRead{Source: testOrigin, ReadAt: 9_000_001, WindowStart: window, Walk: []byte(`{}`),
				PRs: []ForgeHistoryPR{pr(int64(n/2), window+int64(n)*10+1)}})
			t.Logf("%d pull requests: a steady read of one in %v", n, time.Since(start).Round(time.Microsecond))

			start = time.Now()
			rows, err := s.ForgeHistoryByKey(fmt.Sprintf("WEB-%d", n/3))
			if err != nil || len(rows) != 2 {
				t.Fatalf("by key = %d rows, %v", len(rows), err)
			}
			t.Logf("%d pull requests: a key's read in %v", n, time.Since(start).Round(time.Microsecond))

			start = time.Now()
			applyForge(t, s, ForgeHistoryRead{Source: testOrigin, ReadAt: 9_000_002, WindowStart: window + int64(n)*10 + 2, Walk: []byte(`{}`)})
			t.Logf("%d pull requests: the prune of them all in %v", n, time.Since(start).Round(time.Millisecond))
			assertRows(t, s.db, `SELECT count(*) FROM forge_history_prs`, "0")
			assertRows(t, s.db, `SELECT (SELECT count(*) FROM forge_history_acts) + (SELECT count(*) FROM forge_history_keys)`, "0")
		})
	}
}

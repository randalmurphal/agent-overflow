package store

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
)

// agentTreeMarksFromScratch is what every local spawn's marks must hold,
// found the long way: for each spawn that reaches its target, a walk down
// the spawns below that target, counting the open work each thread made at
// each depth (agentOpenWorkSQL). Spawns without open work below are left
// out.
func agentTreeMarksFromScratch(t *testing.T, db sqlQueryer) map[string][agentOpenDepth]int64 {
	t.Helper()
	type link struct{ token, caller, target string }
	var links []link
	rows, err := db.Query(`SELECT token, caller_thread_id, target_thread_id FROM thread_requests r WHERE ` + agentSpawnLinkSQL("r"))
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var l link
		if err := rows.Scan(&l.token, &l.caller, &l.target); err != nil {
			t.Fatal(err)
		}
		links = append(links, l)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	work := map[string]int64{}
	rows, err = db.Query(`SELECT caller_thread_id, COUNT(*) FROM thread_requests r WHERE ` + agentOpenWorkSQL("r") + ` GROUP BY caller_thread_id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var thread string
		var n int64
		if err := rows.Scan(&thread, &n); err != nil {
			t.Fatal(err)
		}
		work[thread] = n
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	children := map[string][]string{}
	for _, l := range links {
		children[l.caller] = append(children[l.caller], l.target)
	}
	want := map[string][agentOpenDepth]int64{}
	for _, l := range links {
		var mark [agentOpenDepth]int64
		level := []string{l.target}
		for d := range agentOpenDepth {
			var next []string
			for _, thread := range level {
				mark[d] += work[thread]
				next = append(next, children[thread]...)
			}
			level = next
		}
		if mark != ([agentOpenDepth]int64{}) {
			want[l.token] = mark
		}
	}
	return want
}

// agentTreeMarks is every request's stored marks, those holding any.
func agentTreeMarks(t *testing.T, db sqlQueryer) map[string][agentOpenDepth]int64 {
	t.Helper()
	rows, err := db.Query(`SELECT token, open_below_0, open_below_1, open_below_2, open_below_3 FROM thread_requests
		WHERE open_below_0 <> 0 OR open_below_1 <> 0 OR open_below_2 <> 0 OR open_below_3 <> 0`)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][agentOpenDepth]int64{}
	for rows.Next() {
		var token string
		var mark [agentOpenDepth]int64
		if err := rows.Scan(&token, &mark[0], &mark[1], &mark[2], &mark[3]); err != nil {
			t.Fatal(err)
		}
		got[token] = mark
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	return got
}

// assertAgentTreeMarks fails when a stored mark differs from the walk's.
func assertAgentTreeMarks(t *testing.T, db sqlQueryer, what string) {
	t.Helper()
	if diff := agentTreeMarksDiff(t, db); diff != "" {
		t.Fatalf("%s: marks differ from the walk:\n%s", what, diff)
	}
}

// agentTreeMarksDiff is each mark that differs from the walk's, or empty.
func agentTreeMarksDiff(t *testing.T, db sqlQueryer) string {
	t.Helper()
	want, got := agentTreeMarksFromScratch(t, db), agentTreeMarks(t, db)
	if maps.Equal(want, got) {
		return ""
	}
	var diff []string
	for _, token := range slices.Sorted(maps.Keys(maps.Collect(func(yield func(string, bool) bool) {
		for token := range want {
			yield(token, true)
		}
		for token := range got {
			yield(token, true)
		}
	}))) {
		if want[token] != got[token] {
			diff = append(diff, fmt.Sprintf("%s: stored %v, walk %v", token, got[token], want[token]))
		}
	}
	return strings.Join(diff, "\n")
}

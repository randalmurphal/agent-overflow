package store

import (
	"strings"
	"testing"
)

// A thread row reads whether its newest turn is open and whether it is an
// unread interruption from one probe of that row, however many flags the
// row derives from it.
func TestAThreadRowProbesItsNewestTurnStateOnce(t *testing.T) {
	s := newTestStore(t)
	plan := explainPlan(t, s, `SELECT `+threadColumns+` FROM threads WHERE id = ?1`, "t")
	var probes []string
	for _, row := range plan {
		if strings.HasPrefix(row.detail, "SEARCH nt ") {
			probes = append(probes, row.detail)
		}
	}
	if len(probes) != 1 || !strings.Contains(probes[0], "turns_thread_index") {
		t.Fatalf("newest-turn probes of a thread row = %q, want one through turns_thread_index\n%s", probes, planText(plan))
	}
}

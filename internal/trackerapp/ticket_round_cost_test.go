package trackerapp

import (
	"fmt"
	"testing"

	"agent-overflow/internal/tracker"
)

// A round of 50 shown tickets that finds 10 of them changed costs, on
// Jira, the one cards read of the check, stored with each ticket's record
// in one ticket write, and one full read of the changed tickets together,
// stored in one more; on Linear, the one read in full of every ticket,
// stored in one ticket write with each card, body and status result. Both
// then build the changed tickets' views from the record: one read of their
// bodies, and one of their cards and names. The
// store calls do not grow with the tickets; the requests these provider
// calls make are TestFakeRoundRequests', the statements and row writes
// the store's TestChangedRoundStatements'.
func TestChangedRoundCosts(t *testing.T) {
	t.Parallel()
	for _, jira := range []bool{true, false} {
		t.Run(map[bool]string{true: "jira", false: "linear"}[jira], func(t *testing.T) {
			t.Parallel()
			m, st, issues, _ := newRoundFixture(t, 50, jira)
			m.round(t)
			for _, id := range issues[:10] {
				st.edit(id, func(c *tracker.Card) { c.Title = "Renamed " + string(id) })
			}
			cs := newCountingStore(m.st)
			m.svc.store = cs
			calls, events := len(m.p.callsInOrder()), m.issueEventCount()
			if res := m.round(t); res.failed {
				t.Fatalf("round %+v", res)
			}
			provider, stored := describeCounts(countsOf(m.p.callsInOrder()[calls:])), describeCounts(cs.take())
			wantProvider := "Cards:1 Details:1"
			wantStore := "GetMirrorCatalog:1 GetMirrorSync:1 ListMirrorBodies:1 ListMirrorCards:1 StoreMirrorTickets:2"
			if !jira {
				wantProvider, wantStore = "Details:1", "GetMirrorCatalog:1 GetMirrorSync:1 ListMirrorBodies:1 ListMirrorCards:1 StoreMirrorTickets:1"
			}
			t.Logf("10 of 50 changed: provider %s, store %s", provider, stored)
			if provider != wantProvider || stored != wantStore {
				t.Errorf("a round with 10 of 50 tickets changed cost provider %s, store %s; pinned %s and %s", provider, stored, wantProvider, wantStore)
			}
			if got := m.issueEventsSince(events); len(got) != 10 {
				t.Errorf("the round told %d tickets; want the 10 changed: %s", len(got), fmt.Sprint(got))
			}
		})
	}
}

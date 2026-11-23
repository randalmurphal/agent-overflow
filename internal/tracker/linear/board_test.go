package linear

import (
	"errors"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/tracker/linear/lineartest"
	"agent-overflow/internal/tracker/trackerapi"
)

const boardSince = `{"gte":"2026-10-01T00:00:00.000Z"}`

var (
	boardWindow = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	boardFilter = `{"team":{"id":{"eq":"team-web"}},"or":[{"isActive":{"eq":true}},{"isFuture":{"eq":true}},{"endsAt":` + boardSince + `},{"completedAt":` + boardSince + `}]}`
	boardRead   = lineartest.Exchange{Op: "TeamBoard", Vars: `{"id":"team-web","teamId":"team-web","first":100,"cycles":` + boardFilter + `,"cycleFirst":20}`}
	moreStates  = lineartest.Exchange{Op: "WorkflowStates", Vars: `{"teamId":"team-web","first":100,"after":"s1"}`}
	moreCycles  = lineartest.Exchange{Op: "Cycles", Vars: `{"first":20,"after":"c1","filter":` + boardFilter + `}`}
	moreUsers   = lineartest.Exchange{Op: "Users", Vars: `{"first":100,"after":"u1","includeDisabled":true}`}
)

func boardState(id string) string {
	return lineartest.State{ID: id, Name: id, Type: "unstarted", Team: "team-web"}.JSON()
}

func boardCycle(id string) string {
	return lineartest.Cycle{ID: id, Number: 1, Team: "team-web", StartsAt: "2026-11-02T00:00:00.000Z", EndsAt: "2026-11-16T00:00:00.000Z"}.JSON()
}

func boardUser(id string) string { return lineartest.User{ID: id, Name: id, Active: true}.JSON() }

// boardAnswer is TeamBoard's answer whose lists hold one node each, every
// one with a page after it.
func boardAnswer(state, cycle, user string) string {
	conn := func(node, cursor string) string { return lineartest.ConnectionJSON([]string{node}, true, cursor) }
	return lineartest.Data(`"team":` + team("WEB") + `,"states":` + conn(boardState(state), "s1") +
		`,"cycles":` + conn(boardCycle(cycle), "c1") + `,"users":` + conn(boardUser(user), "u1"))
}

func lastPage(node string) string {
	return lineartest.Data(lineartest.PageJSON([]string{node}, false, ""))
}

// ids is each item's ID, joined.
func ids[T any](items []T, id func(T) string) string {
	out := make([]string, 0, len(items))
	for _, v := range items {
		out = append(out, id(v))
	}
	return strings.Join(out, ",")
}

func boardIDs(b *Board) string {
	return ids(b.States, func(s WorkflowState) string { return s.ID }) + " " +
		ids(b.Cycles, func(c Cycle) string { return c.ID }) + " " + ids(b.Users, func(u User) string { return u.ID })
}

// A list longer than its first page in TeamBoard's answer continues with
// its own read from that page's cursor, with the page size and filter of
// the first page; the fake refuses any request its script does not hold.
func TestTeamBoardPagesOn(t *testing.T) {
	t.Parallel()
	read := boardRead
	read.Body = boardAnswer("s-a", "c-a", "u-a")
	f := newFake(t, read,
		lineartest.Exchange{Op: moreStates.Op, Vars: moreStates.Vars, Body: lastPage(boardState("s-b"))},
		lineartest.Exchange{Op: moreCycles.Op, Vars: moreCycles.Vars, Body: lastPage(boardCycle("c-b"))},
		lineartest.Exchange{Op: moreUsers.Op, Vars: moreUsers.Vars, Body: lastPage(boardUser("u-b"))},
	)
	b, err := f.client(t).TeamBoard(t.Context(), "team-web", boardWindow)
	if err != nil {
		t.Fatal(err)
	}
	if got := boardIDs(b); got != "s-a,s-b c-a,c-b u-a,u-b" || b.Team.ID != "team-WEB" {
		t.Fatalf("board %s of team %s", got, b.Team.ID)
	}
	if _, err := f.client(t).TeamBoard(t.Context(), "", boardWindow); !errors.Is(err, trackerapi.ErrInvalidInput) {
		t.Errorf("no team: %v", err)
	}
}

// A continuation answered under another key than the first page starts
// the whole read again under that key, so the board is one user's.
func TestTeamBoardUnderOneKey(t *testing.T) {
	t.Parallel()
	f := newFake(t,
		as(boardRead, "a", boardAnswer("s-secret", "c-secret", "u-secret")),
		refusedFor(moreStates, "a"),
		as(moreStates, "b", lastPage(boardState("s-b"))),
		as(boardRead, "b", boardAnswer("s-a", "c-a", "u-a")),
		as(moreStates, "b", lastPage(boardState("s-b"))),
		as(moreCycles, "b", lastPage(boardCycle("c-b"))),
		as(moreUsers, "b", lastPage(boardUser("u-b"))),
	)
	b, err := turnClient(t, f, &keyTurns{}).TeamBoard(t.Context(), "team-web", boardWindow)
	if err != nil {
		t.Fatal(err)
	}
	if got := boardIDs(b); got != "s-a,s-b c-a,c-b u-a,u-b" {
		t.Fatalf("board %s, want b's alone", got)
	}
}

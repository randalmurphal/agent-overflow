package trackerapp

import (
	"testing"
	"time"

	"agent-overflow/internal/harness/trackerfake"
	"agent-overflow/internal/tracker"
)

// TestASharedSprintSettles: a sprint made on board 1 that board 2 lists
// as well is stored on each board with its origin, board 1, and a second
// structure read of either board changes nothing and tells nothing.
func TestASharedSprintSettles(t *testing.T) {
	t.Parallel()
	s := newFakeScenario(t, func(at time.Time) trackerfake.Fixture {
		site := jiraSeed(at)
		second := site.Boards[0]
		second.ID, second.Name = 2, "WEB second board"
		site.Boards = append(site.Boards, second)
		site.Sprints[0].Shown = []int64{2}
		return trackerfake.Fixture{Jira: []trackerfake.JiraSite{site}}
	}, "example.atlassian.net")
	conn := s.addJira()
	s.writeToken(conn, "fake-jira-token-1")
	s.listBoards(conn.ID, "1", "2")
	read := func(board tracker.BoardID) Changes {
		out, err := s.svc.tickStructure(t.Context(), Scope{Connection: conn.ID, Board: board}, Background)
		if err != nil || out.Failure != nil {
			t.Fatalf("structure of board %s: %+v, %v", board, out, err)
		}
		return out.Changes()
	}
	for _, b := range []tracker.BoardID{"1", "2"} {
		if ch := read(b); !ch.Structure {
			t.Fatalf("the first structure read of board %s stored nothing: %+v", b, ch)
		}
	}
	for _, b := range []tracker.BoardID{"1", "2", "1", "2"} {
		if ch := read(b); !ch.Empty() {
			t.Fatalf("a second structure read of board %s changed %+v", b, ch)
		}
	}
	root, _, err := s.st.GetOrCreateMirrorRoot(conn.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range []string{"1", "2"} {
		st, ok, err := s.st.GetMirrorBoard(root, b)
		if err != nil || !ok {
			t.Fatalf("board %s: %v, %v", b, ok, err)
		}
		origins := map[string]string{}
		for _, sp := range st.Sprints {
			origins[sp.ID] = sp.OriginBoardID
		}
		want := map[string]string{"1": "1", "2": "1"}
		if b == "2" {
			want = map[string]string{"1": "1"}
		}
		if len(origins) != len(want) || origins["1"] != want["1"] || origins["2"] != want["2"] {
			t.Errorf("board %s holds sprints by origin %v, want %v", b, origins, want)
		}
	}
}

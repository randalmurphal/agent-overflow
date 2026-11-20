package store

import (
	"testing"
	"time"
)

// peerSpawnOf accepts a paired computer's spawn of target from device "dev",
// made by its thread lead-<token>: a link when link is set, else a spawn
// made with a sidebar row, which carries no level, so it links and tracks
// nothing but its run. Its run is open.
func peerSpawnOf(t *testing.T, s *Store, token, target string, link bool) {
	t.Helper()
	if link {
		acceptLink(t, s, token, "dev", target, 10)
		return
	}
	reachable(t, s, "dev")
	if _, created, err := s.AcceptThreadRequestReceipt(ThreadRequestReceipt{
		Token: token, OwnerDeviceID: "dev", SourceComputerID: "pc-dev", SourceComputerName: "Desk dev",
		SourceThreadID: "lead-" + token, SourceThreadTitle: "Lead " + token, Kind: ThreadRequestSpawn,
		CreatedAt: 10,
	}); err != nil || !created {
		t.Fatalf("accept spawn %s: created %v, %v", token, created, err)
	}
	if named, _, err := s.SetThreadReceiptTarget(token, target, false); err != nil || !named {
		t.Fatalf("name spawn %s: named %v, %v", token, named, err)
	}
}

// peerSendOf accepts a send to target from device "dev" made by source
// there, as its acceptance does.
func peerSendOf(t *testing.T, s *Store, token, source, target string) {
	t.Helper()
	if _, created, err := s.AcceptThreadRequestReceipt(ThreadRequestReceipt{
		Token: token, OwnerDeviceID: "dev", SourceComputerID: "pc-dev", SourceComputerName: "Desk dev",
		SourceThreadID: source, Kind: ThreadRequestSend, TargetThreadID: target, CreatedAt: 30,
	}); err != nil || !created {
		t.Fatalf("accept send %s: created %v, %v", token, created, err)
	}
}

// endPeerRun settles the receipt under token and ends its run, as the
// worker's rest after it does.
func endPeerRun(t *testing.T, s *Store, token string) {
	t.Helper()
	mustExec(t, s.db, `UPDATE thread_request_receipts SET state = 'finished', settled_at = 20,
		run_open = 0, run_tool_calls = 0, run_tokens = 0, run_turn_ms = 0 WHERE token = ?`, token)
}

// peerOwnerToken is the spawn token of the paired computer's thread that
// answers threadID's prompts now, "" for none.
func peerOwnerToken(t *testing.T, s *Store, threadID string) string {
	t.Helper()
	owner, found, err := s.SpawnOwnerOf(threadID)
	if err != nil {
		t.Fatalf("SpawnOwnerOf(%s): %v", threadID, err)
	}
	if !found {
		return ""
	}
	return owner.Token
}

// A worker a paired computer's thread spawned here asks that thread while
// it is owned, and, with a sidebar row, only in a run that thread started:
// its spawn's, or a later send's from the same thread, open on its receipt
// here, whether the spawn made it owned, standalone or grouped. A send from
// another thread there, a run that ended, a released link and a device that
// lost its reach make it the person's.
func TestAPairedSpawnersRunMakesItsSidebarWorkerAskIt(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		link  bool
		place func(t *testing.T, s *Store)
	}{
		{name: "a link shown in the sidebar", link: true, place: func(t *testing.T, s *Store) {
			if _, err := s.SetThreadPlacement([]string{"w"}, PlacementStandalone); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "a link moved into a group", link: true, place: func(t *testing.T, s *Store) {
			group, err := s.CreateThreadGroup(defaultTestProjectID, "Sweep")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.SetThreadGroup([]string{"w"}, group.ID); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "a spawn made with a sidebar row", place: func(*testing.T, *Store) {}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newTestStore(t)
			mustCreateThread(t, s, "w")
			peerSpawnOf(t, s, "spawn", "w", tc.link)
			tc.place(t, s)
			if got := peerOwnerToken(t, s, "w"); got != "spawn" {
				t.Fatalf("during its spawn's run the worker asks %q, want the spawn's thread there", got)
			}
			endPeerRun(t, s, "spawn")
			if got := peerOwnerToken(t, s, "w"); got != "" {
				t.Fatalf("after its spawn's run the worker asks %q, want the person", got)
			}
			// Another thread's send there tracks no run here; one that did
			// would still be no run of the spawning thread's.
			peerSendOf(t, s, "other-send", "lead-other", "w")
			mustExec(t, s.db, `UPDATE thread_request_receipts SET run_open = 1 WHERE token = 'other-send'`)
			if got := peerOwnerToken(t, s, "w"); got != "" {
				t.Fatalf("in another thread's send the worker asks %q, want the person", got)
			}
			mustExec(t, s.db, `UPDATE thread_request_receipts SET run_open = NULL WHERE token = 'other-send'`)
			peerSendOf(t, s, "send", "lead-spawn", "w")
			if got := peerOwnerToken(t, s, "w"); got != "spawn" {
				t.Fatalf("in its spawner's send the worker asks %q, want the spawn's thread there", got)
			}
			endPeerRun(t, s, "send")
			if got := peerOwnerToken(t, s, "w"); got != "" {
				t.Fatalf("after the send's run the worker asks %q, want the person", got)
			}
		})
	}

	t.Run("an owned worker whose runs ended", func(t *testing.T) {
		t.Parallel()
		s := newTestStore(t)
		mustCreateThread(t, s, "w")
		peerSpawnOf(t, s, "spawn", "w", true)
		endPeerRun(t, s, "spawn")
		if got := peerOwnerToken(t, s, "w"); got != "spawn" {
			t.Fatalf("the owned worker asks %q, want its spawner there", got)
		}
	})

	t.Run("a released link", func(t *testing.T) {
		t.Parallel()
		s := newTestStore(t)
		mustCreateThread(t, s, "w")
		peerSpawnOf(t, s, "spawn", "w", true)
		if _, err := s.SetThreadPlacement([]string{"w"}, PlacementStandalone); err != nil {
			t.Fatal(err)
		}
		if released, err := s.ReleaseSpawnLink("spawn", "dev", "lead-spawn"); err != nil || len(released) != 1 {
			t.Fatalf("release: %v, %v", released, err)
		}
		if got := peerOwnerToken(t, s, "w"); got != "" {
			t.Fatalf("after its link's release the worker asks %q, want the person", got)
		}
		if run, err := s.ReceiptRunOf("dev", "lead-spawn", "w"); err != nil || run.Open {
			t.Fatalf("after its link's release the spawn's run reads %+v, %v; want it ended", run, err)
		}
	})

	t.Run("a device that lost its reach", func(t *testing.T) {
		t.Parallel()
		s := newTestStore(t)
		mustCreateThread(t, s, "w")
		peerSpawnOf(t, s, "spawn", "w", false)
		if _, err := s.RevokeDevice("dev", time.Now().UnixMilli()); err != nil {
			t.Fatal(err)
		}
		if got := peerOwnerToken(t, s, "w"); got != "" {
			t.Fatalf("after its device's revocation the worker asks %q, want the person", got)
		}
	})

	// A spawn written before runs were kept is open until it settles.
	t.Run("a link from before runs were kept", func(t *testing.T) {
		t.Parallel()
		s := newTestStore(t)
		mustCreateThread(t, s, "w")
		peerSpawnOf(t, s, "spawn", "w", true)
		if _, err := s.SetThreadPlacement([]string{"w"}, PlacementStandalone); err != nil {
			t.Fatal(err)
		}
		mustExec(t, s.db, `UPDATE thread_request_receipts SET run_open = NULL WHERE token = 'spawn'`)
		if got := peerOwnerToken(t, s, "w"); got != "spawn" {
			t.Fatalf("before it settled the worker asks %q, want the spawn's thread there", got)
		}
		mustExec(t, s.db, `UPDATE thread_request_receipts SET state = 'finished', settled_at = 20 WHERE token = 'spawn'`)
		if got := peerOwnerToken(t, s, "w"); got != "" {
			t.Fatalf("after it settled the worker asks %q, want the person", got)
		}
	})
}

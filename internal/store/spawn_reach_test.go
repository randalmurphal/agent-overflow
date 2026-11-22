package store

import (
	"testing"
)

// ReachSpawnLink reads a spawn's receipt for its own device and source
// thread alone, lineage included, and records the owner's contact with a
// standing link whatever its thread's placement. Whose its thread's prompts
// are is SpawnOwnerOf's to say.
func TestReachSpawnLink(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "worker")
	acceptLink(t, s, "tok", "dev", "worker", 1000)
	at := int64(1000 + 2*spawnLinkTouchGap)
	reach, found, err := s.ReachSpawnLink("tok", "dev", "lead-tok", at)
	if err != nil || !found || !reach.Linked || !reach.Open() || reach.TargetThreadID != "worker" {
		t.Fatalf("the owner's reach = %+v, %v, %v", reach, found, err)
	}
	if seen := receiptLink(t, s, "tok").OwnerSeenAt; seen != at {
		t.Fatalf("owner seen at %d, want %d", seen, at)
	}
	for name, call := range map[string]struct{ token, device, source string }{
		"another device": {"tok", "other", "lead-tok"},
		"another source": {"tok", "dev", "elsewhere"},
		"another token":  {"missing", "dev", "lead-tok"},
	} {
		if _, found, err := s.ReachSpawnLink(call.token, call.device, call.source, at+4*spawnLinkTouchGap); err != nil || found {
			t.Errorf("%s reached the link: %v, %v", name, found, err)
		}
	}
	if seen := receiptLink(t, s, "tok").OwnerSeenAt; seen != at {
		t.Fatalf("another caller moved owner seen to %d", seen)
	}

	// Given a sidebar row and kept as lineage: still the owner's link, its
	// contact still recorded.
	if _, err := s.SetThreadPlacement([]string{"worker"}, PlacementStandalone); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE thread_request_receipts SET lineage_only = 1, state = 'finished', settled_at = 1 WHERE token = 'tok'`); err != nil {
		t.Fatal(err)
	}
	later := at + 2*spawnLinkTouchGap
	reach, found, err = s.ReachSpawnLink("tok", "dev", "lead-tok", later)
	if err != nil || !found || !reach.Linked || reach.Open() || reach.TargetThreadID != "worker" {
		t.Fatalf("the placed lineage's reach = %+v, %v, %v", reach, found, err)
	}
	var seen int64
	if err := s.db.QueryRow(`SELECT owner_seen_at FROM thread_request_receipts WHERE token = 'tok'`).Scan(&seen); err != nil || seen != later {
		t.Fatalf("owner seen at %d, %v; want %d", seen, err, later)
	}
}

// A receipt settled before its thread is named names none, for a paired
// computer's request and a local one alike.
func TestNamingASettledReceiptIsRefused(t *testing.T) {
	s := newTestStore(t)
	for _, id := range []string{"caller", "remote-made", "local-made"} {
		mustCreateThread(t, s, id)
	}
	settle := func(token string) {
		t.Helper()
		if _, settled, err := settleThreadReceiptRow(s.db, token, ThreadReceiptOpenStates(), ThreadRequestSettlement{
			State: ThreadReceiptInterrupted, AnswerKind: ThreadAnswerError, Answer: []byte("interrupted"),
		}); err != nil || !settled {
			t.Fatalf("settle %s: %v, %v", token, settled, err)
		}
	}
	if _, _, err := s.AcceptThreadRequestReceipt(ThreadRequestReceipt{
		Token: "remote", OwnerDeviceID: "dev", SourceThreadID: "lead", Kind: ThreadRequestSpawn,
	}); err != nil {
		t.Fatal(err)
	}
	settle("remote")
	if named, _, err := s.SetThreadReceiptTarget("remote", "remote-made", false); err != nil || named {
		t.Fatalf("naming a settled receipt: %v, %v; want it refused", named, err)
	}
	if _, err := s.AcceptLocalThreadRequest(ThreadRequest{
		Token: "local", CallerThreadID: "caller", Kind: ThreadRequestSpawn, State: ThreadRequestUnconfirmed,
	}, ThreadRequestReceipt{Token: "local", OwnerDeviceID: "device", Kind: ThreadRequestSpawn, SourceThreadID: "caller"}); err != nil {
		t.Fatal(err)
	}
	settle("local")
	if _, err := s.NameLocalThreadRequestTarget("local", "local-made", true); err == nil {
		t.Fatal("a settled local receipt was named")
	}
	for _, token := range []string{"remote", "local"} {
		var target *string
		if err := s.db.QueryRow(`SELECT target_thread_id FROM thread_request_receipts WHERE token = ?`, token).Scan(&target); err != nil || target != nil {
			t.Fatalf("%s names %v, %v; want no thread", token, target, err)
		}
	}
}

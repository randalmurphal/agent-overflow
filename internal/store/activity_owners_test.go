package store

import "testing"

func TestActivityOwnersIncludeAcceptedSendAndAncestorSpawn(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "worker")
	mustCreateThread(t, s, "child")
	acceptLink(t, s, "spawn", "desk", "worker", 1)
	if err := s.InsertThreadRequest(ThreadRequest{Token: "local", CallerThreadID: "worker", Kind: ThreadRequestSpawn, TargetThreadID: "child", State: ThreadRequestRunning}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AcceptThreadRequestReceipt(ThreadRequestReceipt{Token: "send", OwnerDeviceID: "laptop", SourceThreadID: "lead", Kind: ThreadRequestSend, TargetThreadID: "child"}); err != nil {
		t.Fatal(err)
	}
	got, err := s.ActivityOwners(nil, []string{"child"})
	if err != nil || len(got) != 2 || got[0] != (ActivityOwner{"desk", "spawn"}) || got[1] != (ActivityOwner{"laptop", "send"}) {
		t.Fatalf("a prompt on the busy child must notify both owners, got %+v, %v", got, err)
	}
}

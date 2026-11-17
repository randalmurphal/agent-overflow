package store

import (
	"encoding/base64"
	"errors"
	"slices"
	"testing"

	"agent-overflow/internal/owndevices"
)

// acceptLink records, as the destination's acceptance does, a paired
// computer's owned spawn of target: a link receipt from device, created at
// createdAt, then named with own.
func acceptLink(t *testing.T, s *Store, token, device, target string, createdAt int64) {
	t.Helper()
	if _, created, err := s.AcceptThreadRequestReceipt(ThreadRequestReceipt{
		Token: token, OwnerDeviceID: device, SourceComputerID: "pc-" + device, SourceComputerName: "Desk " + device,
		SourceThreadID: "lead-" + token, SourceThreadTitle: "Lead " + token, Kind: ThreadRequestSpawn,
		SpawnLink: true, CreatedAt: createdAt,
	}); err != nil || !created {
		t.Fatalf("accept link %s: created %v, %v", token, created, err)
	}
	if named, _, err := s.SetThreadReceiptTarget(token, target, true); err != nil || !named {
		t.Fatalf("name link %s: named %v, %v", token, named, err)
	}
}

// localSpawnerOf is SpawnOwnerOf for the tests of a local spawner.
func localSpawnerOf(s *Store, threadID string) (spawnerThreadID, title string, found bool, err error) {
	owner, found, err := s.SpawnOwnerOf(threadID)
	return owner.ThreadID, owner.Title, found, err
}

func receiptLink(t *testing.T, s *Store, token string) ThreadRequestReceipt {
	t.Helper()
	row, found, err := s.GetThreadRequestReceipt(token)
	if err != nil || !found {
		t.Fatalf("read receipt %s: found %v, %v", token, found, err)
	}
	return row
}

// A link receipt owns the thread it names: the naming write makes it
// owned, every catalog leaves it out, it can be made owned again only
// while linked, its owner and computer read from the link, and a second
// link for the same thread is refused.
func TestALinkReceiptOwnsItsThread(t *testing.T) {
	s := newTestStore(t)
	for _, id := range []string{"worker", "plain"} {
		mustCreateThread(t, s, id)
		if err := s.InsertItem(Item{ID: id + "-msg", ThreadID: id, Kind: "user_text", Role: "user", Summary: "hi " + id}); err != nil {
			t.Fatal(err)
		}
	}
	acceptLink(t, s, "tok", "dev-s", "worker", 1000)
	if got, _ := placementOf(t, s, "worker"); got != PlacementOwned {
		t.Fatalf("a linked worker = %s, want owned", got)
	}
	row, err := s.GetThread("worker")
	if err != nil || row.SpawnedByComputerID != "pc-dev-s" || row.SpawnedByThreadID != "" {
		t.Fatalf("worker spawned by = %q on %q, %v; want the link's computer and no local spawner", row.SpawnedByThreadID, row.SpawnedByComputerID, err)
	}
	if got := receiptLink(t, s, "tok"); !got.SpawnLink || got.OwnerSeenAt != 1000 || got.OwnerOpSeq != 0 {
		t.Fatalf("receipt = link %v seen %d seq %d; want a link seen at its creation", got.SpawnLink, got.OwnerSeenAt, got.OwnerOpSeq)
	}
	rows, err := s.ListThreadsWithItems()
	if err != nil || slices.ContainsFunc(rows, func(row Thread) bool { return row.ID == "worker" }) {
		t.Fatalf("catalog lists the linked worker: %v", err)
	}
	projects, err := s.ListProjectsWithThreadCounts()
	if err != nil {
		t.Fatal(err)
	}
	for _, project := range projects {
		if project.Project.ID == defaultTestProjectID && project.ThreadCount != 1 {
			t.Fatalf("project count = %d, want the linked worker left out", project.ThreadCount)
		}
	}
	for _, id := range []string{"worker", "plain"} {
		if _, _, err := s.ArchiveThread(id); err != nil {
			t.Fatal(err)
		}
	}
	if archived, err := s.ListArchivedThreads(); err != nil || len(archived) != 1 || archived[0].ID != "plain" {
		t.Fatalf("archived list = %+v, %v; want only plain", archived, err)
	}

	owner, found, err := s.SpawnOwnerOf("worker")
	want := SpawnOwner{ThreadID: "lead-tok", Title: "Lead tok", ComputerID: "pc-dev-s", ComputerName: "Desk dev-s", DeviceID: "dev-s", Token: "tok"}
	if err != nil || !found || owner != want {
		t.Fatalf("SpawnOwnerOf(worker) = %+v, %v, %v; want %+v (an archived worker still has its owner)", owner, found, err, want)
	}

	if _, err := s.SetThreadPlacement([]string{"worker"}, PlacementStandalone); err != nil {
		t.Fatal(err)
	}
	if _, found, err := s.SpawnOwnerOf("worker"); err != nil || found {
		t.Fatalf("SpawnOwnerOf a linked worker with a sidebar row = %v, %v; want none", found, err)
	}
	if row, err := s.GetThread("worker"); err != nil || row.SpawnedByComputerID != "pc-dev-s" {
		t.Fatalf("a linked worker with a sidebar row is spawned on %q, %v; want its link's computer", row.SpawnedByComputerID, err)
	}
	if _, err := s.SetThreadPlacement([]string{"worker"}, PlacementOwned); err != nil {
		t.Fatalf("Remove from Sidebar of a linked worker: %v", err)
	}
	if _, err := s.SetThreadPlacement([]string{"plain"}, PlacementOwned); !errors.Is(err, ErrThreadNotSpawned) {
		t.Fatalf("owning a thread with no link = %v, want ErrThreadNotSpawned", err)
	}

	if _, _, err := s.AcceptThreadRequestReceipt(ThreadRequestReceipt{
		Token: "tok-2", OwnerDeviceID: "dev-s", SourceComputerID: "pc-dev-s", SourceThreadID: "lead", Kind: ThreadRequestSpawn, SpawnLink: true,
	}); err != nil {
		t.Fatal(err)
	}
	if named, _, err := s.SetThreadReceiptTarget("tok-2", "worker", true); err == nil || named {
		t.Fatalf("a second link for one thread = %v, %v; want the unique index to refuse it", named, err)
	}
}

// own owns only through a link: a paired computer's plain spawn names its
// thread and leaves it standalone, own or not.
func TestNamingAPlainReceiptOwnsNothing(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "plain")
	if _, _, err := s.AcceptThreadRequestReceipt(ThreadRequestReceipt{
		Token: "tok", OwnerDeviceID: "dev", SourceComputerID: "pc", SourceThreadID: "lead", Kind: ThreadRequestSpawn,
	}); err != nil {
		t.Fatal(err)
	}
	if named, _, err := s.SetThreadReceiptTarget("tok", "plain", true); err != nil || !named {
		t.Fatalf("name: %v, %v", named, err)
	}
	if got, _ := placementOf(t, s, "plain"); got != PlacementStandalone {
		t.Fatalf("a plain receipt's thread = %s, want standalone", got)
	}
	if _, found, err := s.SpawnOwnerOf("plain"); err != nil || found {
		t.Fatalf("SpawnOwnerOf a plain receipt's thread = %v, %v; want none", found, err)
	}
}

// A link is a spawn from another computer, naming its computer and thread:
// any other receipt marked one is refused, and so is a local request's.
func TestOnlyAPairedSpawnIsALink(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "caller")
	mustCreateThread(t, s, "target")
	for name, receipt := range map[string]ThreadRequestReceipt{
		"a send":             {Token: "send", OwnerDeviceID: "dev", SourceComputerID: "pc", SourceThreadID: "lead", Kind: ThreadRequestSend, TargetThreadID: "target"},
		"a local call":       {Token: "local", OwnerDeviceID: "local", SourceComputerID: "pc", SourceThreadID: "lead", Kind: ThreadRequestSpawn},
		"no source computer": {Token: "nopc", OwnerDeviceID: "dev", SourceThreadID: "lead", Kind: ThreadRequestSpawn},
		"no source thread":   {Token: "nolead", OwnerDeviceID: "dev", SourceComputerID: "pc", Kind: ThreadRequestSpawn},
	} {
		receipt.SpawnLink = true
		if _, _, err := s.AcceptThreadRequestReceipt(receipt); err == nil {
			t.Errorf("%s accepted as a link", name)
		}
	}
	if _, err := s.AcceptLocalThreadRequest(ThreadRequest{
		Token: "loc", CallerThreadID: "caller", Kind: ThreadRequestSpawn, State: ThreadRequestUnconfirmed,
	}, ThreadRequestReceipt{Token: "loc", OwnerDeviceID: "device", SourceComputerID: "pc", SourceThreadID: "caller", Kind: ThreadRequestSpawn, SpawnLink: true}); err == nil {
		t.Error("a local request's receipt accepted as a link")
	}
}

// The trigger: a link that ends, by clearing its flag, detaching its thread
// or deleting the receipt, leaves an owned thread of a project standalone,
// and a thread a local spawn also links stays owned.
func TestALinkEndingLeavesItsThreadStandalone(t *testing.T) {
	s := newTestStore(t)
	for _, id := range []string{"cleared", "detached", "deleted", "local-too", "spawner"} {
		mustCreateThread(t, s, id)
	}
	acceptLink(t, s, "tok-cleared", "dev", "cleared", 1000)
	acceptLink(t, s, "tok-detached", "dev", "detached", 1000)
	acceptLink(t, s, "tok-deleted", "dev", "deleted", 1000)
	acceptLink(t, s, "tok-local-too", "dev", "local-too", 1000)
	if _, err := s.db.Exec(`INSERT INTO thread_requests (token, caller_thread_id, kind, target_computer_id, target_thread_id, state, created_at, updated_at)
		VALUES ('tok-local', 'spawner', 'spawn', '', 'local-too', 'finished', 1, 1)`); err != nil {
		t.Fatal(err)
	}
	for _, write := range []string{
		`UPDATE thread_request_receipts SET spawn_link = 0 WHERE token = 'tok-cleared'`,
		`UPDATE thread_request_receipts SET target_thread_id = NULL WHERE token = 'tok-detached'`,
		`DELETE FROM thread_request_receipts WHERE token = 'tok-deleted'`,
		`UPDATE thread_request_receipts SET spawn_link = 0 WHERE token = 'tok-local-too'`,
	} {
		if _, err := s.db.Exec(write); err != nil {
			t.Fatalf("%s: %v", write, err)
		}
	}
	for id, want := range map[string]string{"cleared": PlacementStandalone, "detached": PlacementStandalone, "deleted": PlacementStandalone, "local-too": PlacementOwned} {
		if got, _ := placementOf(t, s, id); got != want {
			t.Errorf("%s = %s, want %s", id, got, want)
		}
	}
	// A thread the link names is deleted with its receipt: the trigger
	// runs on the cascade and the delete goes ahead.
	acceptLink(t, s, "tok-gone", "dev", "spawner", 1000)
	if err := s.DeleteThread("spawner"); err != nil {
		t.Fatalf("delete a linked thread: %v", err)
	}
}

// The release writer: revoking a device releases exactly that device's
// links; the retention sweep releases a link whose owner has been silent
// past the floor and keeps a fresh link of the same device, and a released
// thread cannot be owned again.
func TestReleasingLinks(t *testing.T) {
	s, owner, device := seedOwnerDevice(t)
	other, err := s.CreateDevice(owner.ID, "Other", "desktop", "linux")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"mine", "theirs", "stale", "fresh"} {
		mustCreateThread(t, s, id)
	}
	acceptLink(t, s, "tok-mine", device.ID, "mine", 1000)
	acceptLink(t, s, "tok-theirs", other.ID, "theirs", 1000)
	acceptLink(t, s, "tok-stale", other.ID, "stale", 1000)
	acceptLink(t, s, "tok-fresh", other.ID, "fresh", 1000)
	if _, _, err := s.AcceptThreadRequestReceipt(ThreadRequestReceipt{
		Token: "tok-unnamed", OwnerDeviceID: device.ID, SourceComputerID: "pc", SourceThreadID: "lead", Kind: ThreadRequestSpawn, SpawnLink: true, CreatedAt: 1000,
	}); err != nil {
		t.Fatal(err)
	}

	revoked, err := s.RevokeDevice(device.ID, 2000)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(revoked.ReleasedThreads, []string{"mine"}) {
		t.Fatalf("revocation released %v, want [mine]", revoked.ReleasedThreads)
	}
	for token, want := range map[string]bool{"tok-mine": false, "tok-unnamed": false, "tok-theirs": true} {
		if got := receiptLink(t, s, token).SpawnLink; got != want {
			t.Errorf("%s link = %v after the revocation, want %v", token, got, want)
		}
	}
	if got, _ := placementOf(t, s, "mine"); got != PlacementStandalone {
		t.Fatalf("a revoked device's worker = %s, want standalone", got)
	}
	if _, err := s.SetThreadPlacement([]string{"mine"}, PlacementOwned); !errors.Is(err, ErrThreadNotSpawned) {
		t.Fatalf("owning a released worker = %v, want ErrThreadNotSpawned", err)
	}

	if touched, err := s.TouchSpawnLinkOwner("tok-fresh", other.ID, "lead-tok-fresh", 1000+2*spawnLinkTouchGap); err != nil || !touched {
		t.Fatalf("touch the fresh link: %v, %v", touched, err)
	}
	_, released, err := s.DeleteThreadRequestsBefore(1000 + spawnLinkTouchGap)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(released)
	if !slices.Equal(released, []string{"stale", "theirs"}) {
		t.Fatalf("the sweep released %v, want the silent links [stale theirs]", released)
	}
	for id, want := range map[string]string{"stale": PlacementStandalone, "theirs": PlacementStandalone, "fresh": PlacementOwned} {
		if got, _ := placementOf(t, s, id); got != want {
			t.Errorf("%s after the sweep = %s, want %s", id, got, want)
		}
	}
	if _, released, err := s.DeleteThreadRequestsBefore(1000 + spawnLinkTouchGap); err != nil || len(released) != 0 {
		t.Fatalf("a second sweep released %v, %v; want none", released, err)
	}
}

// A merge that withdraws this computer from the personal group revokes every
// member's personal session here, and so releases the links of the devices
// those sessions were opened under; a merge this computer stays through
// leaves them, and a device paired outside the group keeps its links.
func TestLeavingThePersonalGroupReleasesTheMembersLinks(t *testing.T) {
	s, link := approvedOwnPair(t)
	if e := s.AdmitOwnPairing(link); e != nil {
		t.Fatal(e)
	}
	session, err := s.GetSession(link.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	stranger, err := s.CreateDevice(session.UserID, "Stranger", "desktop", "linux")
	if err != nil {
		t.Fatal(err)
	}
	mustCreateThread(t, s, "member-worker")
	mustCreateThread(t, s, "stranger-worker")
	acceptLink(t, s, "tok-member", session.DeviceID, "member-worker", 1000)
	acceptLink(t, s, "tok-stranger", stranger.ID, "stranger-worker", 1000)

	const backendID = "93fd7522-831e-4401-8aa0-6dca4e65bcb0"
	key := make([]byte, 32)
	key[0] = 2
	host := owndevices.Member{KeyThumbprint: base64.RawURLEncoding.EncodeToString(key), BackendID: backendID, Generation: 1}
	if changed, revoked, released, err := s.MergeOwnDevices([]owndevices.Member{host}, 2100, backendID); err != nil || !changed || len(revoked) != 0 || len(released) != 0 {
		t.Fatalf("joining this computer = changed %v revoked %v released %v, %v; want the member's links kept", changed, revoked, released, err)
	}
	if got, _ := placementOf(t, s, "member-worker"); got != PlacementOwned {
		t.Fatalf("a member's worker after a merge this computer stays through = %s, want owned", got)
	}

	host.Removed = true
	changed, revoked, released, err := s.MergeOwnDevices([]owndevices.Member{host}, 2200, backendID)
	if err != nil || !changed || !slices.Equal(revoked, []string{link.SessionID}) {
		t.Fatalf("leaving the group = changed %v revoked %v, %v; want the member's session revoked", changed, revoked, err)
	}
	if !slices.Equal(released, []string{"member-worker"}) {
		t.Fatalf("leaving the group released %v, want [member-worker]", released)
	}
	for id, want := range map[string]string{"member-worker": PlacementStandalone, "stranger-worker": PlacementOwned} {
		if got, _ := placementOf(t, s, id); got != want {
			t.Errorf("%s after leaving the group = %s, want %s", id, got, want)
		}
	}
}

// A merge that moves a member to a new generation revokes its old session
// while the member and this computer stay in the group: its device keeps
// its pairing, and its links.
func TestANewGenerationKeepsTheMembersLinks(t *testing.T) {
	s, link := approvedOwnPair(t)
	if e := s.AdmitOwnPairing(link); e != nil {
		t.Fatal(e)
	}
	session, err := s.GetSession(link.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	mustCreateThread(t, s, "member-worker")
	acceptLink(t, s, "tok-member", session.DeviceID, "member-worker", 1000)
	member, err := s.OwnDeviceForSession(link.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	member.Generation++
	changed, revoked, released, err := s.MergeOwnDevices([]owndevices.Member{member}, 2200, "93fd7522-831e-4401-8aa0-6dca4e65bcb0")
	if err != nil || !changed || !slices.Equal(revoked, []string{link.SessionID}) {
		t.Fatalf("a new generation = changed %v revoked %v, %v; want the old session revoked", changed, revoked, err)
	}
	if len(released) != 0 {
		t.Fatalf("a new generation released %v, want none", released)
	}
	if got, _ := placementOf(t, s, "member-worker"); got != PlacementOwned {
		t.Fatalf("a member's worker after a new generation = %s, want owned", got)
	}
}

// A revocation or a removal that cannot release its device's links changes
// nothing: the release is part of its transaction.
func TestAnAccessLossThatCannotReleaseItsLinksChangesNothing(t *testing.T) {
	s, link := approvedOwnPair(t)
	if e := s.AdmitOwnPairing(link); e != nil {
		t.Fatal(e)
	}
	session, err := s.GetSession(link.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	mustCreateThread(t, s, "worker")
	acceptLink(t, s, "tok", session.DeviceID, "worker", 1000)
	if _, err := s.db.Exec(`CREATE TRIGGER refuse_release BEFORE UPDATE OF spawn_link ON thread_request_receipts BEGIN SELECT RAISE(ABORT, 'disk failure'); END`); err != nil {
		t.Fatal(err)
	}
	member, err := s.OwnDeviceForSession(link.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	member.Removed = true
	if _, _, _, err := s.MergeOwnDevices([]owndevices.Member{member}, 2200, ""); err == nil {
		t.Fatal("a removal went ahead without releasing its links")
	}
	if held, err := s.OwnDeviceForSession(link.SessionID); err != nil || held.Removed {
		t.Fatalf("the member was removed without releasing its links: %+v, %v", held, err)
	}
	if _, err := s.RevokeDevice(session.DeviceID, 2300); err == nil {
		t.Fatal("a revocation went ahead without releasing its links")
	}
	if got, err := s.GetSession(link.SessionID); err != nil || got.RevokedAt != 0 {
		t.Fatalf("the session was revoked without releasing its links: %+v, %v", got, err)
	}
	if got, _ := placementOf(t, s, "worker"); got != PlacementOwned {
		t.Fatalf("worker = %s, want still owned", got)
	}
}

// A link no newer than its creation is not past the floor however silent
// its owner: the floor reads both.
func TestTheSweepKeepsAYoungLink(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "young")
	acceptLink(t, s, "tok", "dev", "young", 5000)
	if _, err := s.db.Exec(`UPDATE thread_request_receipts SET owner_seen_at = 0 WHERE token = 'tok'`); err != nil {
		t.Fatal(err)
	}
	if _, released, err := s.DeleteThreadRequestsBefore(4000); err != nil || len(released) != 0 {
		t.Fatalf("the sweep released %v, %v; want a link younger than the floor kept", released, err)
	}
}

// A merge that removes a member releases the links of the devices its
// sessions were opened under, in the merge's transaction: proven through
// the pairing records a real own-device admission writes.
func TestMergeRemovingAMemberReleasesItsLinks(t *testing.T) {
	s, link := approvedOwnPair(t)
	if e := s.AdmitOwnPairing(link); e != nil {
		t.Fatal(e)
	}
	session, err := s.GetSession(link.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	stranger, err := s.CreateDevice(session.UserID, "Stranger", "desktop", "linux")
	if err != nil {
		t.Fatal(err)
	}
	mustCreateThread(t, s, "member-worker")
	mustCreateThread(t, s, "stranger-worker")
	acceptLink(t, s, "tok-member", session.DeviceID, "member-worker", 1000)
	acceptLink(t, s, "tok-stranger", stranger.ID, "stranger-worker", 1000)
	member, err := s.OwnDeviceForSession(link.SessionID)
	if err != nil {
		t.Fatal(err)
	}

	// A merge that changes the member without removing it releases nothing.
	renamed := member
	renamed.Name = "Renamed"
	if changed, _, released, err := s.MergeOwnDevices([]owndevices.Member{renamed}, 2150, ""); err != nil || !changed || len(released) != 0 {
		t.Fatalf("a rename merge = changed %v released %v, %v; want nothing released", changed, released, err)
	}

	member.Removed = true
	changed, _, released, err := s.MergeOwnDevices([]owndevices.Member{member}, 2200, "")
	if err != nil || !changed {
		t.Fatalf("merge: %v, %v", changed, err)
	}
	if !slices.Equal(released, []string{"member-worker"}) {
		t.Fatalf("the removal released %v, want [member-worker]", released)
	}
	for id, want := range map[string]string{"member-worker": PlacementStandalone, "stranger-worker": PlacementOwned} {
		if got, _ := placementOf(t, s, id); got != want {
			t.Errorf("%s = %s, want %s", id, got, want)
		}
	}
	// A link the device makes after its removal, through a pairing outside
	// the group, is not the removal's: a later merge that removes nobody
	// new leaves it.
	mustCreateThread(t, s, "later-worker")
	acceptLink(t, s, "tok-later", session.DeviceID, "later-worker", 3000)
	key := make([]byte, 32)
	key[0] = 1
	joined := owndevices.Member{KeyThumbprint: base64.RawURLEncoding.EncodeToString(key), Generation: 1}
	if changed, _, released, err := s.MergeOwnDevices([]owndevices.Member{joined}, 3100, ""); err != nil || !changed || len(released) != 0 {
		t.Fatalf("a join merge = changed %v released %v, %v; want nothing released", changed, released, err)
	}
	if got, _ := placementOf(t, s, "later-worker"); got != PlacementOwned {
		t.Fatalf("a link made after the removal = %s after a later merge, want owned", got)
	}
}

// Past the floor a link receipt is kept as lineage with its link, its
// owner's last word and its operation sequence.
func TestLineageKeepsTheLink(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "worker")
	acceptLink(t, s, "tok", "dev", "worker", 1000)
	if _, err := s.db.Exec(`UPDATE thread_request_receipts SET state = 'finished', settled_at = 1000, owner_seen_at = 9000, owner_op_seq = 3 WHERE token = 'tok'`); err != nil {
		t.Fatal(err)
	}
	if _, released, err := s.DeleteThreadRequestsBefore(5000); err != nil || len(released) != 0 {
		t.Fatalf("sweep released %v, %v", released, err)
	}
	var lineage, link bool
	var seen, seq int64
	if err := s.db.QueryRow(`SELECT lineage_only, spawn_link, owner_seen_at, owner_op_seq FROM thread_request_receipts WHERE token = 'tok'`).Scan(&lineage, &link, &seen, &seq); err != nil {
		t.Fatal(err)
	}
	if !lineage || !link || seen != 9000 || seq != 3 {
		t.Fatalf("lineage %v link %v seen %d seq %d; want the lineage to keep the link, 9000 and 3", lineage, link, seen, seq)
	}
	if got, _ := placementOf(t, s, "worker"); got != PlacementOwned {
		t.Fatalf("a lineage-only link's worker = %s, want owned", got)
	}
}

// TouchSpawnLinkOwner records the owner's call only for the link's own
// device and source, and at most once an hour.
func TestTouchSpawnLinkOwner(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "worker")
	acceptLink(t, s, "tok", "dev", "worker", 1000)
	for name, touch := range map[string]struct{ device, source string }{
		"another device": {"other", "lead-tok"},
		"another source": {"dev", "elsewhere"},
	} {
		if touched, err := s.TouchSpawnLinkOwner("tok", touch.device, touch.source, 1000+2*spawnLinkTouchGap); err != nil || touched {
			t.Errorf("%s touched the link: %v, %v", name, touched, err)
		}
	}
	if touched, err := s.TouchSpawnLinkOwner("tok", "dev", "lead-tok", 1000+spawnLinkTouchGap/2); err != nil || touched {
		t.Fatalf("a touch inside the hour wrote: %v, %v", touched, err)
	}
	at := int64(1000 + 2*spawnLinkTouchGap)
	if touched, err := s.TouchSpawnLinkOwner("tok", "dev", "lead-tok", at); err != nil || !touched {
		t.Fatalf("the owner's touch: %v, %v", touched, err)
	}
	if got := receiptLink(t, s, "tok").OwnerSeenAt; got != at {
		t.Fatalf("owner seen at %d, want %d", got, at)
	}
	if _, err := s.ReleaseSpawnLink("tok", "dev", "lead-tok"); err != nil {
		t.Fatal(err)
	}
	if touched, err := s.TouchSpawnLinkOwner("tok", "dev", "lead-tok", at+2*spawnLinkTouchGap); err != nil || touched {
		t.Fatalf("a released link was touched: %v, %v", touched, err)
	}
}

// ReleaseSpawnLink ends one link for its own device and source.
func TestReleaseSpawnLink(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "worker")
	acceptLink(t, s, "tok", "dev", "worker", 1000)
	for name, release := range map[string]struct{ device, source string }{
		"another device": {"other", "lead-tok"},
		"another source": {"dev", "elsewhere"},
	} {
		if released, err := s.ReleaseSpawnLink("tok", release.device, release.source); err != nil || len(released) != 0 {
			t.Errorf("%s released %v, %v", name, released, err)
		}
	}
	if released, err := s.ReleaseSpawnLink("tok", "dev", "lead-tok"); err != nil || !slices.Equal(released, []string{"worker"}) {
		t.Fatalf("release = %v, %v; want [worker]", released, err)
	}
	if got, _ := placementOf(t, s, "worker"); got != PlacementStandalone {
		t.Fatalf("a released worker = %s, want standalone", got)
	}
}

// SupersedeLink raises the link's operation sequence and never lowers it.
func TestSupersedeLink(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "worker")
	acceptLink(t, s, "tok", "dev", "worker", 1000)
	if found, err := s.SupersedeLink("tok", "other", "lead-tok", 9); err != nil || found {
		t.Fatalf("another device superseded: %v, %v", found, err)
	}
	for _, seq := range []int64{4, 2} {
		if found, err := s.SupersedeLink("tok", "dev", "lead-tok", seq); err != nil || !found {
			t.Fatalf("supersede %d: %v, %v", seq, found, err)
		}
	}
	if got := receiptLink(t, s, "tok").OwnerOpSeq; got != 4 {
		t.Fatalf("owner op seq = %d, want 4", got)
	}
}

// SpawnOwnerOf names a local spawner first, as SpawnerOf did, and a
// paired computer's link only for a thread still owned.
func TestSpawnOwnerOf(t *testing.T) {
	s := newTestStore(t)
	for _, id := range []string{"lead", "local", "archived-lead", "orphan", "remote"} {
		mustCreateThread(t, s, id)
	}
	spawnLocal(t, s, "tok-local", "lead", "local", true)
	spawnLocal(t, s, "tok-orphan", "archived-lead", "orphan", true)
	if _, _, err := s.ArchiveThread("archived-lead"); err != nil {
		t.Fatal(err)
	}
	acceptLink(t, s, "tok-remote", "dev", "remote", 1000)
	for id, want := range map[string]SpawnOwner{
		"local":  {ThreadID: "lead", Title: "Thread lead"},
		"remote": {ThreadID: "lead-tok-remote", Title: "Lead tok-remote", ComputerID: "pc-dev", ComputerName: "Desk dev", DeviceID: "dev", Token: "tok-remote"},
	} {
		if got, found, err := s.SpawnOwnerOf(id); err != nil || !found || got != want {
			t.Errorf("SpawnOwnerOf(%s) = %+v, %v, %v; want %+v", id, got, found, err, want)
		}
	}
	for _, id := range []string{"orphan", "lead"} {
		if got, found, err := s.SpawnOwnerOf(id); err != nil || found {
			t.Errorf("SpawnOwnerOf(%s) = %+v, %v, %v; want none", id, got, found, err)
		}
	}
}

// A delete admitted through a link claims its worker only while the link
// still owns it: the token and its device, and the thread owned.
func TestALinkFencesADeleteClaim(t *testing.T) {
	s := newTestStore(t)
	for _, id := range []string{"worker", "placed"} {
		mustCreateThread(t, s, id)
	}
	acceptLink(t, s, "tok", "dev", "worker", 1000)
	acceptLink(t, s, "tok-placed", "dev", "placed", 1000)
	if _, err := s.SetThreadPlacement([]string{"placed"}, PlacementStandalone); err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	for name, claim := range map[string]struct {
		id    string
		fence LinkFence
	}{
		"another device":   {"worker", LinkFence{Token: "tok", DeviceID: "other"}},
		"another token":    {"worker", LinkFence{Token: "tok-placed", DeviceID: "dev"}},
		"a sidebar thread": {"placed", LinkFence{Token: "tok-placed", DeviceID: "dev"}},
	} {
		fence := claim.fence
		if err := claimThreadDeleteTx(tx, "op", claim.id, deleteFence{link: &fence}); !errors.Is(err, ErrWorkerNotOwned) {
			t.Errorf("%s: claim = %v, want ErrWorkerNotOwned", name, err)
		}
	}
	if err := claimThreadDeleteTx(tx, "op", "worker", deleteFence{link: &LinkFence{Token: "tok", DeviceID: "dev"}}); err != nil {
		t.Fatalf("the link's claim: %v", err)
	}
}

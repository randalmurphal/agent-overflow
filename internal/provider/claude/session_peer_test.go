package claude

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"agent-overflow/internal/provider"
)

// The ledger is what tells a turn this app asked for from a turn another
// Claude session started, so both directions are load-bearing — and the
// asymmetry between `issued` (non-consuming) and `release` (consuming) is
// the part that a naive implementation gets wrong: one uuid produces a
// whole bracket, and every frame of it must classify the same way.
func TestIssuedCommandUUIDsIssuedDoesNotConsume(t *testing.T) {
	var l issuedCommandUUIDs
	l.note("abc")
	for i := 0; i < 3; i++ {
		if !l.issued("abc") {
			t.Fatalf("issued() went false on read %d — a bracket's frames would disagree", i+1)
		}
	}
	l.release("abc")
	if l.issued("abc") {
		t.Fatal("issued() still true after release")
	}
	if l.overflowed() {
		t.Fatal("overflowed() true on a healthy ledger")
	}
}

func TestIssuedCommandUUIDsIgnoresBlankAndDuplicates(t *testing.T) {
	var l issuedCommandUUIDs
	l.note("")
	l.note("   ")
	if l.issued("") || l.issued("   ") {
		t.Fatal("blank uuid recorded")
	}
	l.note("dup")
	l.note("dup")
	l.release("dup")
	if l.issued("dup") {
		t.Fatal("a duplicate note created a second entry that survived one release")
	}
}

// At the cap the ledger refuses NEW entries rather than evicting old
// ones, and it latches `overflowed` — which is what makes the classifier
// fail safe. Evicting instead would silently re-label an older, still
// in-flight send as peer-originated.
func TestIssuedCommandUUIDsRefusesAtCapAndLatchesOverflow(t *testing.T) {
	var l issuedCommandUUIDs
	for i := 0; i < maxTrackedIssuedCommandUUIDs; i++ {
		l.note(uuidLike(i))
	}
	if l.overflowed() {
		t.Fatal("overflowed() true at exactly the cap")
	}
	l.note("one-too-many")
	if l.issued("one-too-many") {
		t.Fatal("entry admitted past the cap")
	}
	if !l.issued(uuidLike(0)) {
		t.Fatal("the oldest entry was evicted; the cap must refuse, not evict")
	}
	if !l.overflowed() {
		t.Fatal("overflow not latched")
	}
	// Latched, not sampled: draining the ledger must not restore the
	// claim that an absent uuid was never issued.
	for i := 0; i < maxTrackedIssuedCommandUUIDs; i++ {
		l.release(uuidLike(i))
	}
	if !l.overflowed() {
		t.Fatal("overflow un-latched after a drain")
	}
}

// Mislabelling a peer turn as ours costs a missing attribution label.
// Mislabelling OUR turn as a peer's puts "another Claude session sent
// this" on a message the user typed, which is the transcript lying about
// who asked for what. The classifier is biased accordingly.
func TestCommandUUIDIsPeerOriginatedFailsSafe(t *testing.T) {
	s := &Session{}
	if s.commandUUIDIsPeerOriginated("") {
		t.Fatal("empty uuid classified as peer-originated")
	}
	if !s.commandUUIDIsPeerOriginated("cli-minted") {
		t.Fatal("a uuid this app never issued must classify as peer-originated")
	}
	s.noteIssuedCommandUUID("ours")
	if s.commandUUIDIsPeerOriginated("ours") {
		t.Fatal("an AO-issued uuid classified as peer-originated")
	}
	// Under overflow nothing can be PROVEN unissued, so every unknown
	// uuid reverts to "ours".
	for i := 0; i <= maxTrackedIssuedCommandUUIDs; i++ {
		s.noteIssuedCommandUUID(uuidLike(i))
	}
	if s.commandUUIDIsPeerOriginated("cli-minted") {
		t.Fatal("an overflowed ledger still claimed a uuid was unissued")
	}
}

// The parser stamps the origin on EVERY frame of the bracket, and
// releases only on the terminal one — so a consumer reading `completed`
// reaches the same verdict as one reading `started`.
func TestParseCommandLifecycleStampsPeerOriginAcrossTheBracket(t *testing.T) {
	sess := &Session{}
	sess.noteIssuedCommandUUID("ours-1")
	p := NewParser()
	p.peerTurns = sess

	for _, tc := range []struct {
		commandUUID string
		wantOrigin  string
	}{
		{"ours-1", ""},
		{"peer-1", PeerTurnOrigin},
	} {
		for _, state := range []string{"queued", "started", "completed"} {
			line := []byte(`{"type":"command_lifecycle","command_uuid":"` + tc.commandUUID + `","state":"` + state + `"}`)
			events, err := p.ParseLine(testThread, line)
			if err != nil {
				t.Fatalf("ParseLine(%s/%s): %v", tc.commandUUID, state, err)
			}
			if len(events) != 1 {
				t.Fatalf("ParseLine(%s/%s) = %d events, want 1", tc.commandUUID, state, len(events))
			}
			var meta provider.CommandLifecycleMeta
			if err := json.Unmarshal(events[0].Meta, &meta); err != nil {
				t.Fatalf("unmarshal meta: %v", err)
			}
			if meta.Origin != tc.wantOrigin {
				t.Fatalf("%s/%s origin = %q, want %q", tc.commandUUID, state, meta.Origin, tc.wantOrigin)
			}
		}
	}

	// The terminal frame released the AO entry, so the ledger is not a
	// leak. (A later frame for the same uuid would now read as a peer's —
	// which cannot happen, because a bracket terminates once.)
	if sess.issuedCommands.issued("ours-1") {
		t.Fatal("terminal lifecycle state did not release the ledger entry")
	}
}

// A bare Parser is a supported construction — every parser unit test
// builds one — and must read as "cannot classify" rather than panic or
// invent a peer.
func TestParseCommandLifecycleWithoutAClassifierStampsNoOrigin(t *testing.T) {
	p := NewParser()
	events, err := p.ParseLine(testThread, []byte(`{"type":"command_lifecycle","command_uuid":"x","state":"started"}`))
	if err != nil {
		t.Fatalf("ParseLine: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	var meta provider.CommandLifecycleMeta
	if err := json.Unmarshal(events[0].Meta, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if meta.Origin != "" {
		t.Fatalf("origin = %q on a classifier-less parser, want empty", meta.Origin)
	}
}

func TestRenamePeerSessionRefusedWhenCrossSessionIsOff(t *testing.T) {
	s := &Session{}
	err := s.RenamePeerSession(context.Background(), "Anything")
	if !errors.Is(err, ErrPeerRenameUnavailable) {
		t.Fatalf("RenamePeerSession on a non-peer session = %v, want ErrPeerRenameUnavailable", err)
	}
}

// An empty `/rename` argument makes the CLI answer "That name is empty
// once invisible characters are removed" and change nothing — an error
// that would surface here as a success. Refuse before the write.
func TestRenamePeerSessionRefusesAnEmptySanitizedName(t *testing.T) {
	s := &Session{crossSessionEnabled: true}
	if err := s.RenamePeerSession(context.Background(), "​ \t "); err == nil {
		t.Fatal("RenamePeerSession(invisible-only) = nil, want refusal")
	}
}

// Skipping the redundant rename is what keeps the idle-edge sync from
// writing a `/rename` after every single turn.
//
// The skip compares REQUESTS (peerRenameSettledName, the last name the CLI
// consumed), not the confirmed peer-visible name: the question it answers is
// "would sending this again change anything", and the CLI's answer to a
// repeat of a request it already took is the answer it gave the first time.
func TestRenamePeerSessionSkipsWhenTheNameAlreadyMatches(t *testing.T) {
	s := &Session{
		crossSessionEnabled:   true,
		peerSessionName:       "AO Thread One",
		peerRenameSettledName: "AO Thread One",
	}
	// Compared AFTER sanitization on both sides, so a name the CLI would
	// normalize into the current one is also a skip. A real send here
	// would nil-panic on the absent process, which is the assertion.
	if err := s.RenamePeerSession(context.Background(), "  AO Thread​One  "); err != nil {
		t.Fatalf("RenamePeerSession(equivalent name) = %v, want nil", err)
	}
}

// The same skip on the case the confirmed/requested split exists for: the CLI
// YIELDED, so the session answers to a name AO never asked for. Re-deriving
// the skip from the confirmed name would send one `/rename` per turn boundary
// for the whole life of that session — the CLI would keep answering "held by
// another live session", AO would keep not matching, and the loop would never
// close.
func TestRenamePeerSessionSkipsARequestTheCLIAnsweredUnderAnotherName(t *testing.T) {
	s := &Session{
		crossSessionEnabled: true,
		// What peers address: the variant the CLI chose on the collision.
		peerSessionName: "AO Thread One (2)",
		// What AO asked for and the CLI consumed.
		peerRenameSettledName: "AO Thread One",
	}
	if err := s.RenamePeerSession(context.Background(), "AO Thread One"); err != nil {
		t.Fatalf("RenamePeerSession(already-requested name) = %v, want a skip", err)
	}
}

// The rename goes out as an ordinary stdin user message carrying AO's own
// client-minted uuid — that uuid is what lets triage's pending-send
// correlator consume the send instead of stranding it (a stranded entry
// poisons turn indexing for the rest of the session, incident
// 2026-08-04) — and it must reach the CLI's slash router unprefixed.
func TestRenamePeerSessionWritesTheSlashCommandWithAnIssuedUUID(t *testing.T) {
	scriptPath := filepath.Join(t.TempDir(), "fake-claude")
	capturePath := filepath.Join(t.TempDir(), "stdin.ndjson")
	script := `#!/bin/sh
set -eu
capture="${CAPTURE_FILE:?}"
while IFS= read -r line; do
    printf '%s\n' "$line" >> "$capture"
done
`
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake claude script: %v", err)
	}

	s, err := NewSession(context.Background(), testThread, Config{
		Binary:              scriptPath,
		CrossSessionEnabled: true,
		PeerSessionName:     "old-name",
		Env:                 map[string]string{"CAPTURE_FILE": capturePath},
	}, func(provider.ProviderEvent) {})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer s.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := s.RenamePeerSession(ctx, "AO Thread One"); err != nil {
		t.Fatalf("RenamePeerSession: %v", err)
	}

	lines := waitCapturedLines(t, capturePath, 1)
	var envelope struct {
		Type    string `json:"type"`
		UUID    string `json:"uuid"`
		Message struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &envelope); err != nil {
		t.Fatalf("unmarshal captured line %q: %v", lines[0], err)
	}
	if envelope.Type != "user" {
		t.Fatalf("captured envelope type = %q, want user", envelope.Type)
	}
	if len(envelope.Message.Content) == 0 {
		t.Fatalf("captured envelope carried no content: %s", lines[0])
	}
	text := envelope.Message.Content[0].Text
	if text != "/rename AO Thread One" {
		// A leading newline here is the slash-guard prefix, which would
		// make the CLI's router miss the line and send it to the model as
		// prose. Native slash routing is the zero-value send behavior.
		t.Fatalf("captured text = %q, want %q", text, "/rename AO Thread One")
	}
	if envelope.UUID == "" {
		t.Fatal("rename envelope carried no client-minted uuid")
	}
	// Registered BEFORE the write, so the bracket the CLI is about to emit
	// cannot be mistaken for a peer's.
	if s.commandUUIDIsPeerOriginated(envelope.UUID) {
		t.Fatal("the rename's own uuid classifies as peer-originated")
	}
	// The write is not the rename. The registry moves when the CLI's own
	// `/rename` command COMPLETES, so the cached name still reports what
	// peers can actually address until that frame lands.
	if got := s.PeerSessionName(); got != "old-name" {
		t.Fatalf("PeerSessionName before the completed frame = %q, want the old name", got)
	}
	// Nor is the completed frame, on its own. The frame carries no name, so
	// the confirmed peer-visible name moves only for output that stated one
	// — which is what keeps a yielded rename from being cached as if AO's
	// request had been honoured.
	s.notePeerRenameOutput(envelope.UUID, "Session renamed to: AO Thread One")
	s.settlePeerRename(envelope.UUID, provider.CommandCompleted)
	if got := s.PeerSessionName(); got != "AO Thread One" {
		t.Fatalf("PeerSessionName after the completed frame = %q", got)
	}
}

// A `/rename` whose bracket ends cancelled or discarded never ran: the peer
// registry still holds the OLD name. Caching the new one would make
// PeerSessionName report an address no peer can reach AND make every later
// reconcile a no-op, because syncPeerSessionName skips a rename whose wanted
// name already matches — the thread would never be renamed again.
func TestSettlePeerRenameKeepsTheOldNameOnANonCompletedBracket(t *testing.T) {
	for _, state := range []provider.CommandLifecycleState{
		provider.CommandCancelled,
		provider.CommandDiscarded,
	} {
		t.Run(string(state), func(t *testing.T) {
			s := &Session{
				crossSessionEnabled: true,
				peerSessionName:     "old-name",
				peerRenameSeq:       1,
				pendingPeerRenames: map[string]pendingPeerRename{
					"rename-1": {name: "new-name", seq: 1},
				},
			}
			s.settlePeerRename("rename-1", state)
			if got := s.PeerSessionName(); got != "old-name" {
				t.Fatalf("PeerSessionName after %s = %q, want the old name", state, got)
			}
			// And the entry is gone, so the next idle point retries rather
			// than treating the rename as still in flight forever.
			if len(s.pendingPeerRenames) != 0 {
				t.Fatalf("pending rename survived a %s bracket: %v", state, s.pendingPeerRenames)
			}
		})
	}
}

// A terminal frame for a SUPERSEDED rename must not promote its stale name
// over the one now in flight — the same identity guard the parser's
// activeCommandUUID keeps.
func TestSettlePeerRenameIgnoresASupersededBracket(t *testing.T) {
	s := &Session{
		crossSessionEnabled: true,
		peerSessionName:     "old-name",
		peerRenameSeq:       2,
		pendingPeerRenames: map[string]pendingPeerRename{
			"rename-2": {name: "newest-name", assigned: "newest-name", seq: 2},
		},
	}
	// rename-1 already settled and was superseded; its frame arrives twice
	// or late. It is not in the pending map at all, so it cannot promote.
	s.settlePeerRename("rename-1", provider.CommandCompleted)
	if got := s.PeerSessionName(); got != "old-name" {
		t.Fatalf("a superseded bracket promoted a name: %q", got)
	}
	if _, ok := s.pendingPeerRenames["rename-2"]; !ok {
		t.Fatal("a superseded bracket cleared the live pending rename")
	}
	s.settlePeerRename("rename-2", provider.CommandCompleted)
	if got := s.PeerSessionName(); got != "newest-name" {
		t.Fatalf("PeerSessionName after the live bracket completed = %q", got)
	}
}

// Both renames are in flight at once — the single-slot version could only
// ever track one of them, so the OLDER frame found an empty (or foreign)
// slot and was dropped while the NEWER one had already been overwritten.
// Each frame must now resolve its own rename, and the session must end on
// the one staged last no matter which order the frames arrive in.
func TestSettlePeerRenameResolvesEachRenameOnItsOwnFrame(t *testing.T) {
	for _, order := range [][]string{{"rename-1", "rename-2"}, {"rename-2", "rename-1"}} {
		t.Run(strings.Join(order, ","), func(t *testing.T) {
			s := &Session{
				crossSessionEnabled: true,
				peerSessionName:     "old-name",
				peerRenameSeq:       2,
				pendingPeerRenames: map[string]pendingPeerRename{
					"rename-1": {name: "first", assigned: "first", seq: 1},
					"rename-2": {name: "second", assigned: "second", seq: 2},
				},
			}
			for _, id := range order {
				s.settlePeerRename(id, provider.CommandCompleted)
			}
			if got := s.PeerSessionName(); got != "second" {
				t.Fatalf("PeerSessionName = %q, want the last-staged rename regardless of frame order", got)
			}
			if len(s.pendingPeerRenames) != 0 {
				t.Fatalf("pending renames survived their frames: %v", s.pendingPeerRenames)
			}
		})
	}
}

// The dedup check reads the newest STAGED rename, not just the confirmed
// name: with two renames in flight the session is heading for the newer
// one, so re-asking for the older one is a real change and must be sent.
func TestRenamePeerSessionTargetsTheNewestStagedRename(t *testing.T) {
	s := &Session{
		crossSessionEnabled: true,
		peerSessionName:     "old-name",
		peerRenameSeq:       2,
		pendingPeerRenames: map[string]pendingPeerRename{
			"rename-1": {name: "first", seq: 1},
			"rename-2": {name: "second", seq: 2},
		},
	}
	s.peerNameMu.Lock()
	target := s.peerRenameTargetLocked()
	s.peerNameMu.Unlock()
	if target != "second" {
		t.Fatalf("rename target = %q, want the newest staged rename", target)
	}
	// No process: a re-send of the newest name would panic on nil stdin.
	if err := s.RenamePeerSession(context.Background(), "second"); err != nil {
		t.Fatalf("RenamePeerSession for the newest staged name: %v", err)
	}
}

// The turn-boundary reconcile calls RenamePeerSession on EVERY completed
// turn, so an in-flight rename must not re-send the same command on each one.
func TestRenamePeerSessionSkipsAnInFlightRename(t *testing.T) {
	s := &Session{
		crossSessionEnabled: true,
		peerSessionName:     "old-name",
		peerRenameSeq:       1,
		pendingPeerRenames: map[string]pendingPeerRename{
			"rename-1": {name: "new-name", seq: 1},
		},
	}
	// No process, no stdin: reaching Send at all would fail this test with a
	// nil-pointer panic rather than an assertion, which is the point.
	if err := s.RenamePeerSession(context.Background(), "new-name"); err != nil {
		t.Fatalf("RenamePeerSession for the in-flight name: %v", err)
	}
}

func uuidLike(i int) string {
	return "uuid-" + strconv.Itoa(i)
}

// The turn-complete idle edge calls into the peer-rename path on every
// completed Claude turn, so "did this session join the peer network" has
// to be answerable without touching the store or the process.
func TestCrossSessionEnabledReportsTheSpawnDecision(t *testing.T) {
	if (&Session{}).CrossSessionEnabled() {
		t.Fatal("a session spawned without the inbox reports it enabled")
	}
	if !(&Session{crossSessionEnabled: true}).CrossSessionEnabled() {
		t.Fatal("a session spawned with the inbox reports it disabled")
	}
}

// A Send whose stdin write FAILS never put its uuid on the wire, so no
// command_lifecycle bracket will ever arrive to consume it. Leaving the
// entry behind leaks one per failed write against a 256-entry cap — and
// once the cap latches, `overflowed` makes every peer-started turn for the
// rest of the session read as local, which is the transcript silently
// dropping "from another Claude session" attribution.
func TestSendReleasesTheIssuedUUIDWhenTheWriteFails(t *testing.T) {
	scriptDir := t.TempDir()
	scriptPath := filepath.Join(scriptDir, "mock-claude")
	if err := os.WriteFile(scriptPath, []byte("#!/bin/bash\nexec cat\n"), 0o755); err != nil {
		t.Fatalf("write mock script: %v", err)
	}
	s, err := NewSession(context.Background(), testThread, Config{Binary: scriptPath}, func(provider.ProviderEvent) {})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer s.Close()

	// Kill the process so every subsequent WriteLine fails at the `done`
	// gate, which is what a wedged or exited CLI looks like to Send.
	_ = s.proc.Kill()
	<-s.proc.Done()

	// A real uuid: Send validates the shape before it records anything, so
	// a placeholder string would fail early and never exercise the write.
	id := uuid.NewString()
	if err := s.Send(context.Background(), "hello", provider.SendOptions{UserMessageUUID: id}); err == nil {
		t.Fatal("Send to a dead process returned no error")
	}
	if s.issuedCommands.issued(id) {
		t.Fatal("a uuid that never reached stdin stayed in the issued ledger")
	}
	if s.issuedCommands.overflowed() {
		t.Fatal("one failed write overflowed the ledger")
	}

	// The leak is only visible at scale, so drive it: more failed writes
	// than the cap must still leave the ledger empty and unlatched.
	for range maxTrackedIssuedCommandUUIDs + 8 {
		_ = s.Send(context.Background(), "hello", provider.SendOptions{UserMessageUUID: uuid.NewString()})
	}
	if s.issuedCommands.overflowed() {
		t.Fatalf("%d failed writes latched the ledger's overflow — every peer turn would read as local for the rest of the session",
			maxTrackedIssuedCommandUUIDs+8)
	}
	s.issuedCommands.mu.Lock()
	left := len(s.issuedCommands.uuids)
	s.issuedCommands.mu.Unlock()
	if left != 0 {
		t.Fatalf("%d issued-ledger entries survived their failed writes", left)
	}
}

// Two concurrent renames must not be able to end with the CLI on one name
// and the cache on another.
//
// Staging alone is not enough to serialize: stage A, stage B, write B,
// write A leaves the CLI answering to A while the cache (which promotes
// the newest STAGED rename) says B — and because a rename whose wanted
// name already matches is skipped, nothing ever corrects it for the life
// of the session. Send's own stdin mutex orders the writes in call order,
// so the fix is to make stage-and-write one critical section: whichever
// rename stages last is then also the last one on the wire.
func TestConcurrentRenamePeerSessionEndsOnTheSameNameTheWireDoes(t *testing.T) {
	dir := t.TempDir()
	wireLog := filepath.Join(dir, "wire.log")
	scriptPath := filepath.Join(dir, "mock-claude")
	script := "#!/bin/bash\nwhile IFS= read -r line; do printf '%s\\n' \"$line\" >> " + wireLog + "; done\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock script: %v", err)
	}

	for attempt := range 25 {
		s, err := NewSession(context.Background(), testThread, Config{
			Binary:              scriptPath,
			CrossSessionEnabled: true,
		}, func(provider.ProviderEvent) {})
		if err != nil {
			t.Fatalf("NewSession: %v", err)
		}
		if err := os.WriteFile(wireLog, nil, 0o644); err != nil {
			t.Fatalf("reset wire log: %v", err)
		}

		const renamers = 8
		var start sync.WaitGroup
		var done sync.WaitGroup
		start.Add(1)
		for i := range renamers {
			done.Add(1)
			go func() {
				defer done.Done()
				start.Wait()
				if err := s.RenamePeerSession(context.Background(), "name-"+strconv.Itoa(i)); err != nil {
					t.Errorf("RenamePeerSession: %v", err)
				}
			}()
		}
		start.Done()
		done.Wait()

		// Settle every bracket as completed, in an order deliberately
		// unrelated to staging order — a terminal frame must resolve its
		// own rename, and promotion must stay monotonic in staging order.
		s.peerNameMu.Lock()
		ids := make([]string, 0, len(s.pendingPeerRenames))
		staged := make(map[string]string, len(s.pendingPeerRenames))
		for id, pending := range s.pendingPeerRenames {
			ids = append(ids, id)
			staged[id] = pending.name
		}
		s.peerNameMu.Unlock()
		sort.Strings(ids)
		for _, id := range ids {
			// The CLI's own confirmation, which is where the peer-visible
			// name is read back from. Each bracket confirms ITS OWN name,
			// exactly as the wire does.
			s.notePeerRenameOutput(id, "Session renamed to: "+staged[id])
			s.settlePeerRename(id, provider.CommandCompleted)
		}

		wireName := lastRenameOnWire(t, wireLog, renamers)
		if got := s.PeerSessionName(); got != wireName {
			t.Fatalf("attempt %d: cache says %q, the CLI ended on %q — every later reconcile would skip the correction",
				attempt, got, wireName)
		}
		s.Close()
	}
}

// lastRenameOnWire reads the mock CLI's stdin log and returns the name of
// the LAST `/rename` that reached it, which is the name the real CLI would
// have ended up registered under.
func lastRenameOnWire(t *testing.T, path string, want int) string {
	t.Helper()
	var lines []string
	// The mock drains stdin on its own schedule; wait for every write.
	for range 200 {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read wire log: %v", err)
		}
		lines = lines[:0]
		for _, line := range strings.Split(string(raw), "\n") {
			if strings.Contains(line, "/rename ") {
				lines = append(lines, line)
			}
		}
		if len(lines) >= want {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(lines) != want {
		t.Fatalf("wire log holds %d renames, want %d", len(lines), want)
	}
	last := lines[len(lines)-1]
	var env struct {
		Message struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal([]byte(last), &env); err != nil {
		t.Fatalf("decode wire line %q: %v", last, err)
	}
	for _, block := range env.Message.Content {
		if name, ok := strings.CutPrefix(strings.TrimSpace(block.Text), "/rename "); ok {
			return name
		}
	}
	t.Fatalf("no /rename text in wire line %q", last)
	return ""
}

// ------------------------------------------------- the rename read-back
//
// `/rename` does not always do what it was asked. On a name collision the CLI
// YIELDS: it registers a variant of its own choosing and reports both names.
// The lifecycle bracket carries no name at all, so a completed frame is not
// evidence about WHICH name this session now answers to — and committing the
// requested one there puts an address no peer can reach into the field whose
// whole job is to hold the reachable one.
//
// The strings below are the CLI's own (2.1.237 `performRename`), recovered
// from the installed bundle. They are read back only for output arriving
// inside a `/rename` AO itself sent, inside that command's own lifecycle
// bracket — never as a classifier over arbitrary provider text.

func TestParsePeerRenameAssignedNameReadsTheCLIsOwnReplies(t *testing.T) {
	cases := []struct {
		name string
		text string
		want string
	}{
		{"plain success", "Session renamed to: AO Thread One", "AO Thread One"},
		{
			"yielded to a live peer",
			`Session renamed to: AO Thread One (2) ("AO Thread One" is held by another live session on this machine)`,
			"AO Thread One (2)",
		},
		{
			"superseded by a newer rename",
			"Session is named: Something Else (a newer rename landed first)",
			"Something Else",
		},
		{"trailing whitespace", "Session renamed to: AO Thread One\n", "AO Thread One"},

		// Refusals name no assigned name, and must not be pattern-matched
		// into one: reading a name out of them would cache a name that was
		// never set.
		{"teammate refusal", "Cannot rename: This session is a teammate.", ""},
		{"empty-name refusal", "That name is empty once invisible characters are removed.", ""},
		{"generation failure", "Could not generate a name: rate limited.", ""},
		{"unrelated output", "Current effort level: high", ""},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parsePeerRenameAssignedName(tc.text); got != tc.want {
				t.Fatalf("parsePeerRenameAssignedName(%q) = %q, want %q", tc.text, got, tc.want)
			}
		})
	}
}

// The collision case end to end: the CLI answered under a name AO did not
// ask for, so that is the name peers address — and the REQUEST still settles,
// so the turn-boundary reconcile does not re-send the same rename forever.
func TestSettlePeerRenamePromotesTheNameTheCLIAssignedNotTheOneAsked(t *testing.T) {
	s := &Session{
		crossSessionEnabled:   true,
		peerSessionName:       "old-name",
		peerRenameSettledName: "old-name",
		pendingPeerRenames: map[string]pendingPeerRename{
			"rename-1": {name: "AO Thread One", seq: 1},
		},
	}
	s.notePeerRenameOutput("rename-1",
		`Session renamed to: AO Thread One (2) ("AO Thread One" is held by another live session on this machine)`)
	s.settlePeerRename("rename-1", provider.CommandCompleted)

	if got := s.PeerSessionName(); got != "AO Thread One (2)" {
		t.Fatalf("PeerSessionName = %q, want the name the CLI actually registered", got)
	}
	// And the request is settled, so asking for the same thing again is a
	// no-op rather than one `/rename` per turn boundary for the session's
	// whole life.
	if err := s.RenamePeerSession(context.Background(), "AO Thread One"); err != nil {
		t.Fatalf("re-requesting the settled name = %v, want a skip", err)
	}
}

// A completed rename whose reply this parser does not recognise: the registry
// may well hold the requested name now, but AO did not see the CLI say so, and
// a peer-visible ADDRESS is not a thing to assume. The confirmed name is left
// alone; the REQUEST still settles, because the alternative is re-sending the
// same unparseable command forever.
func TestSettlePeerRenameKeepsTheConfirmedNameWhenNothingWasReadBack(t *testing.T) {
	s := &Session{
		crossSessionEnabled:   true,
		peerSessionName:       "old-name",
		peerRenameSettledName: "old-name",
		pendingPeerRenames: map[string]pendingPeerRename{
			"rename-1": {name: "AO Thread One", seq: 1},
		},
	}
	s.notePeerRenameOutput("rename-1", "Cannot rename: This session is a teammate.")
	s.settlePeerRename("rename-1", provider.CommandCompleted)

	if got := s.PeerSessionName(); got != "old-name" {
		t.Fatalf("PeerSessionName = %q, want the last CONFIRMED name — the CLI never confirmed a new one", got)
	}
	if err := s.RenamePeerSession(context.Background(), "AO Thread One"); err != nil {
		t.Fatalf("the unconfirmed request was not settled (%v), so the reconcile would re-send it every turn", err)
	}
}

// The read-back is scoped to its own bracket: output arriving under a uuid
// that names no pending rename changes nothing, and one rename's confirmation
// can never land on another's.
func TestNotePeerRenameOutputIsScopedToItsOwnCommandUUID(t *testing.T) {
	s := &Session{
		crossSessionEnabled: true,
		peerSessionName:     "old-name",
		pendingPeerRenames: map[string]pendingPeerRename{
			"rename-1": {name: "first", seq: 1},
			"rename-2": {name: "second", seq: 2},
		},
	}
	s.notePeerRenameOutput("some-other-command", "Session renamed to: Not A Rename")
	s.notePeerRenameOutput("", "Session renamed to: Also Not A Rename")
	s.notePeerRenameOutput("rename-2", "Session renamed to: second")

	s.peerNameMu.Lock()
	first := s.pendingPeerRenames["rename-1"]
	second := s.pendingPeerRenames["rename-2"]
	s.peerNameMu.Unlock()
	if first.assigned != "" {
		t.Fatalf("rename-1 picked up %q from another command's output", first.assigned)
	}
	if second.assigned != "second" {
		t.Fatalf("rename-2 assigned = %q, want its own confirmation", second.assigned)
	}

	s.settlePeerRename("rename-1", provider.CommandCompleted)
	if got := s.PeerSessionName(); got != "old-name" {
		t.Fatalf("an unconfirmed rename promoted %q", got)
	}
	s.settlePeerRename("rename-2", provider.CommandCompleted)
	if got := s.PeerSessionName(); got != "second" {
		t.Fatalf("PeerSessionName = %q, want the confirmed rename", got)
	}
}

// The read-back runs through the same name mirror as every other name here,
// so a CLI reply carrying characters the peer registry would normalize cannot
// put an unnormalized value in the cache.
func TestPeerRenameReadBackIsSanitized(t *testing.T) {
	s := &Session{
		crossSessionEnabled: true,
		pendingPeerRenames: map[string]pendingPeerRename{
			"rename-1": {name: "AO Thread One", seq: 1},
		},
	}
	s.notePeerRenameOutput("rename-1", "Session renamed to: AO​Thread One")
	s.settlePeerRename("rename-1", provider.CommandCompleted)
	got := s.PeerSessionName()
	if got != SanitizePeerSessionName(got) {
		t.Fatalf("PeerSessionName = %q, which is not what the CLI's own normalizer produces", got)
	}
}

// `/rename` is AO's bookkeeping, so its "Session renamed to: …" output must
// not land in the user's transcript — while the bracket still settles and the
// name still promotes. One send, both properties.
func TestRenamePeerSessionMarksItsOutputRowSuppressed(t *testing.T) {
	scriptPath := filepath.Join(t.TempDir(), "fake-claude")
	script := "#!/bin/sh\nwhile IFS= read -r line; do :; done\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake claude script: %v", err)
	}
	s, err := NewSession(context.Background(), testThread, Config{
		Binary:              scriptPath,
		CrossSessionEnabled: true,
	}, func(provider.ProviderEvent) {})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer s.Close()

	if err := s.RenamePeerSession(context.Background(), "AO Thread One"); err != nil {
		t.Fatalf("RenamePeerSession: %v", err)
	}
	s.peerNameMu.Lock()
	ids := make([]string, 0, len(s.pendingPeerRenames))
	for id := range s.pendingPeerRenames {
		ids = append(ids, id)
	}
	s.peerNameMu.Unlock()
	if len(ids) != 1 {
		t.Fatalf("pending renames = %v, want exactly one", ids)
	}
	// Unconditional: the rename is AO's own command, so it is suppressed
	// whatever the CLI answers — including a collision variant the read-back
	// below then promotes.
	if !s.commandResultRowSuppressed(ids[0], "Session renamed to: AO Thread One") {
		t.Fatal("the rename's output row was not marked suppressed at send time")
	}

	// Suppressed, and still settled: the row is the only thing removed.
	s.notePeerRenameOutput(ids[0], "Session renamed to: AO Thread One")
	s.settlePeerRename(ids[0], provider.CommandCompleted)
	if got := s.PeerSessionName(); got != "AO Thread One" {
		t.Fatalf("PeerSessionName = %q — suppression must not cost the rename its promotion", got)
	}
}

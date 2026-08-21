package claude

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"

	"github.com/google/uuid"

	"agent-overflow/internal/provider"
)

// session_peer.go — Agent Overflow's half of Claude Code's cross-session
// messaging ("harbor kite"): telling a peer-started turn apart from one
// this app asked for, and keeping the thread's peer-visible name current.
//
// Two facts about the wire decide the shape of everything here, both
// spike-verified against 2.1.237 under AO's OWN flag set on 2026-08-21
// (/tmp/spike-xsession/logs/q6, q7):
//
//  1. A delivered peer message opens a turn with a `command_lifecycle`
//     bracket whose `command_uuid` AO NEVER MINTED — the CLI minted it
//     for the injected user row. Every AO-issued command_uuid is a uuid
//     AO put on an outbound envelope, so "did we issue this" is the whole
//     classifier. That is what issuedCommandUUIDs answers.
//  2. Because AO always spawns with `--replay-user-messages`, the message
//     itself ALSO reaches stdout, as a `user{isReplay:true,
//     isSynthetic:true}` envelope carrying a structured `origin` object
//     (`{kind:"peer", from, name, body, ...}`) and — critically — a
//     top-level `uuid` EQUAL to the bracket's `command_uuid`. The body is
//     therefore on the wire; nothing has to read the session transcript
//     to recover it. parse_user_replay.go owns that half.

// PeerTurnOrigin is the typed origin marker stamped on a turn this
// session did not start, when the starter was another Claude session on
// this machine.
//
// It is deliberately the same FIELD (`Meta.origin`) and the same concept
// as Codex's ExternalTurnOriginQueue, so the frontend renders "someone
// else put this here" once rather than per provider — and deliberately a
// DIFFERENT VALUE, because the two differ in ways a reader cares about: a
// Codex queue entry was written by the user's own `codex queue`, while
// this one was written by another model. Folding them into a single
// "external" would erase that.
const PeerTurnOrigin = "peer-session"

// maxTrackedIssuedCommandUUIDs bounds the issued-uuid ledger.
//
// Entries are released by the terminal lifecycle state, so a healthy
// session holds a handful (one per in-flight send). The cap only matters
// for sends whose bracket never terminates — a provider crash mid-turn,
// or a CLI old enough to emit no command_lifecycle at all, where the
// entry is never released because nothing ever arrives to release it.
//
// At the cap the ledger REFUSES NEW ENTRIES rather than evicting old
// ones, and the direction is chosen: a refused entry makes AO's own next
// turn look peer-started (a wrong label on a row), while evicting the
// oldest would do the same thing to an OLDER send that may still be in
// flight. Neither is good; the refusal at least cannot un-track a send
// that is still running. A log line marks the transition so the cap
// showing up in the wild is visible rather than inferred.
const maxTrackedIssuedCommandUUIDs = 256

// issuedCommandUUIDs is the ledger of uuids this app stamped on outbound
// user envelopes and has not yet seen terminate.
//
// Concurrency: Send runs on caller goroutines and the lifecycle parse
// runs on the read loop, so this is mutex-guarded — unlike the Parser's
// own correlation maps, which are read-loop-only.
type issuedCommandUUIDs struct {
	mu       sync.Mutex
	uuids    map[string]struct{}
	overflow bool
}

// note records an outbound uuid. Called BEFORE the write reaches stdin,
// so the bracket can never beat the ledger entry onto the read loop.
func (l *issuedCommandUUIDs) note(uuid string) {
	uuid = strings.TrimSpace(uuid)
	if uuid == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.uuids == nil {
		l.uuids = make(map[string]struct{}, 4)
	}
	if _, seen := l.uuids[uuid]; seen {
		return
	}
	if len(l.uuids) >= maxTrackedIssuedCommandUUIDs {
		if !l.overflow {
			// Once per session. The cap doc promises a log line marking the
			// transition, and it is the only way the cap showing up in the
			// wild is visible rather than inferred from turns that suddenly
			// stopped being labelled peer-originated.
			l.overflow = true
			log.Printf(
				"claude: issued-command ledger hit its %d-entry cap; further sends cannot be proven ours, so no turn will be labelled peer-originated for the rest of this session",
				maxTrackedIssuedCommandUUIDs)
		}
		return
	}
	l.uuids[uuid] = struct{}{}
}

// issued reports whether AO minted this uuid. Note the asymmetry with
// release: `issued` does NOT consume, because one uuid produces a whole
// bracket (queued, started, completed) and every frame must classify the
// same way. Consumption happens once, at the terminal state.
func (l *issuedCommandUUIDs) issued(uuid string) bool {
	if uuid == "" {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_, ok := l.uuids[uuid]
	return ok
}

// release drops a uuid once its bracket reaches a terminal state.
func (l *issuedCommandUUIDs) release(uuid string) {
	if uuid == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.uuids, uuid)
}

// overflowed reports whether the ledger ever refused an entry. Used only
// to keep the "unissued uuid" log honest: under overflow, "AO never
// issued this" stops being provable.
func (l *issuedCommandUUIDs) overflowed() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.overflow
}

// noteIssuedCommandUUID records a uuid AO is about to put on the wire, so
// the command_lifecycle bracket it produces is not mistaken for a peer's.
func (s *Session) noteIssuedCommandUUID(uuid string) {
	s.issuedCommands.note(uuid)
}

// commandUUIDIsPeerOriginated reports whether a command_lifecycle frame
// belongs to a turn another Claude session started.
//
// FAIL-SAFE DIRECTION: a uuid is peer-originated only when the ledger
// positively lacks it AND has never overflowed. Mislabelling a peer turn
// as AO's costs a missing attribution label; mislabelling AO's OWN turn
// as a peer's would put "from another Claude session" on a message the
// user typed, which is the transcript lying about who asked for what.
func (s *Session) commandUUIDIsPeerOriginated(uuid string) bool {
	if uuid == "" {
		return false
	}
	if s.issuedCommands.issued(uuid) {
		return false
	}
	return !s.issuedCommands.overflowed()
}

// releaseIssuedCommandUUID drops a uuid whose bracket has terminated.
func (s *Session) releaseIssuedCommandUUID(uuid string) {
	s.issuedCommands.release(uuid)
}

// peerTurnClassifier is the slice of Session the command_lifecycle parser
// needs. An interface rather than a back-pointer because a zero-value
// Parser is a supported construction (every parser unit test builds one),
// and a nil field there must read as "cannot classify" rather than
// panic — which is also the correct answer: with no session ledger, no
// uuid can be PROVEN unissued.
type peerTurnClassifier interface {
	commandUUIDIsPeerOriginated(commandUUID string) bool
	releaseIssuedCommandUUID(commandUUID string)
	settlePeerRename(commandUUID string, state provider.CommandLifecycleState)
	notePeerRenameOutput(commandUUID, text string)
	commandResultRowSuppressed(commandUUID, text string) bool
	releaseSuppressedCommandResult(commandUUID string)
}

// settlePeerRename resolves an in-flight `/rename` against its bracket's
// terminal frame.
//
// Only `completed` settles anything. `cancelled` and `discarded` both mean
// the command never ran — the queue was cleared, the consuming turn aborted,
// or the session ended with it still queued — so the peer registry still
// holds the OLD name and the request is still worth re-sending.
//
// A completed rename settles TWO different facts, and conflating them is what
// let AO cache a name the CLI never agreed to:
//
//   - `peerSessionName` — what peers actually address. Promoted ONLY from the
//     name read back out of the command's own output (notePeerRenameOutput).
//     The lifecycle frame carries no name, and the CLI does not always use
//     the one AO asked for: on a collision it yields to a variant of its own
//     choosing and reports `Session renamed to: X ("Y" is held by another
//     live session on this machine)`. Committing Y there would put a name no
//     peer can reach into the field that claims to hold the reachable one.
//   - `peerRenameSettledName` — the last name AO REQUESTED and the CLI
//     consumed. Settled whether or not a name was read back, because that is
//     the question the send-side dedup asks: re-sending the identical request
//     would produce the identical answer, so a rename whose output AO could
//     not parse must not turn into one `/rename` per turn boundary forever.
//
// Each in-flight rename resolves against ITS OWN pending entry (keyed by
// command uuid), and promotion is monotonic in staging order — together
// those keep a late terminal frame for a superseded rename from promoting
// a stale name over a newer one, and keep a newer rename's frame from
// being swallowed because an older one happened to hold a single slot.
//
// Relying on the bracket is safe here specifically: the peer inbox requires
// 2.1.224+, and every CLI that has it emits command_lifecycle
// (`msg_lifecycle_v1`). The general "absence of command_lifecycle proves
// nothing" rule is about older CLIs, which cannot reach this code at all.
func (s *Session) settlePeerRename(commandUUID string, state provider.CommandLifecycleState) {
	if commandUUID == "" {
		return
	}
	s.peerNameMu.Lock()
	defer s.peerNameMu.Unlock()
	pending, ok := s.pendingPeerRenames[commandUUID]
	if !ok {
		return
	}
	delete(s.pendingPeerRenames, commandUUID)
	if state != provider.CommandCompleted {
		return
	}
	if pending.seq <= s.peerNameSeq {
		// A late frame for a rename an already-promoted one superseded.
		// Its name is what the CLI answered to at the time, not now.
		return
	}
	s.peerNameSeq = pending.seq
	s.peerRenameSettledName = pending.name
	if pending.assigned == "" {
		// The command completed and said nothing this parser recognises as an
		// assigned name. The registry may well hold `pending.name` now, but
		// AO did not see it say so — and a peer-visible ADDRESS is not a
		// thing to assume. Left unpromoted: PeerSessionName keeps reporting
		// the last name that WAS confirmed, and the settled-request record
		// above is what stops this from re-sending every turn.
		log.Printf(
			"claude: /rename to %q completed without a recognised confirmation; the peer-visible name is left as %q",
			pending.name, s.peerSessionName)
		return
	}
	s.peerSessionName = pending.assigned
}

// The confirmations `/rename` answers with, verbatim from the CLI bundle
// (2.1.237, `performRename`): a plain success, a success that YIELDED to
// another live session holding the requested name, and one that was
// SUPERSEDED by a newer rename landing first. In all three the name after the
// colon is the name this session ends up answering to — which is exactly what
// the lifecycle frame does not carry.
//
//	Session renamed to: X
//	Session renamed to: X ("Y" is held by another live session on this machine)
//	Session is named: X (a newer rename landed first)
//
// Every other reply (`Cannot rename: This session is a teammate.`, `That name
// is empty once invisible characters are removed.`, `Could not generate a
// name: …`) names no assigned name, and is deliberately not pattern-matched
// into one: those are refusals, and reading a name out of them would cache a
// name that was never set.
const (
	peerRenameAssignedPrefix   = "Session renamed to: "
	peerRenameSupersededPrefix = "Session is named: "
	peerRenameSupersededSuffix = " (a newer rename landed first)"
	peerRenameYieldedOpen      = ` ("`
	peerRenameYieldedSuffix    = `" is held by another live session on this machine)`
)

// parsePeerRenameAssignedName reads the name a completed `/rename` says this
// session now answers to, or "" when the reply names none.
//
// This is a READ-BACK of AO's own command inside AO's own lifecycle bracket,
// not a classifier over arbitrary provider text: notePeerRenameOutput only
// consults it for output that arrived between the `started` and `completed`
// frames of a uuid AO minted for a `/rename` it sent. Nothing routes on it,
// and an unrecognised reply degrades to "unconfirmed" rather than to a guess.
func parsePeerRenameAssignedName(text string) string {
	text = strings.TrimSpace(text)
	if rest, ok := strings.CutPrefix(text, peerRenameSupersededPrefix); ok {
		return strings.TrimSpace(strings.TrimSuffix(rest, peerRenameSupersededSuffix))
	}
	rest, ok := strings.CutPrefix(text, peerRenameAssignedPrefix)
	if !ok {
		return ""
	}
	if strings.HasSuffix(rest, peerRenameYieldedSuffix) {
		// `X ("Y" is held by …)`. The requested name is quoted, so the LAST
		// ` ("` is the parenthetical's opener even when the assigned name
		// contains one of its own.
		if open := strings.LastIndex(rest, peerRenameYieldedOpen); open >= 0 {
			rest = rest[:open]
		}
	}
	return strings.TrimSpace(rest)
}

// notePeerRenameOutput records the name a `/rename` reported back, against the
// pending rename that asked for it.
//
// Called from the assistant parser for `<synthetic>` command output, which the
// CLI emits INSIDE the issuing command's own started -> completed window
// (parse_command_lifecycle.go owns that window). The uuid is therefore the
// correlation — the text is only ever read for a command AO knows it sent as a
// rename, and a uuid naming no pending rename does nothing at all.
func (s *Session) notePeerRenameOutput(commandUUID, text string) {
	if strings.TrimSpace(commandUUID) == "" {
		return
	}
	assigned := SanitizePeerSessionName(parsePeerRenameAssignedName(text))
	if assigned == "" {
		return
	}
	s.peerNameMu.Lock()
	defer s.peerNameMu.Unlock()
	pending, ok := s.pendingPeerRenames[commandUUID]
	if !ok {
		return
	}
	pending.assigned = assigned
	s.pendingPeerRenames[commandUUID] = pending
}

// pendingPeerRename is one in-flight `/rename`: the name it asked for, the
// name the CLI reported back (empty until its output arrives, and empty for
// good on a reply this parser does not recognise), and the staging order that
// decides which of several concurrent renames the session ends on.
type pendingPeerRename struct {
	name     string
	assigned string
	seq      uint64
}

// peerRenameTargetLocked reports the name this session has most recently ASKED
// to be called — the newest staged rename, or the last settled request when
// nothing is pending. Caller holds peerNameMu.
//
// Deliberately the REQUEST and not peerSessionName: this answers "would
// sending this name again change anything", and the CLI's answer to a repeat
// of a request it already consumed is the same answer it gave the first time.
// Comparing against the confirmed name instead would re-send a `/rename` on
// every turn boundary for the whole life of a session whose name yielded to a
// peer, which is the loop the confirmed/requested split exists to avoid.
func (s *Session) peerRenameTargetLocked() string {
	name, best := s.peerRenameSettledName, s.peerNameSeq
	for _, pending := range s.pendingPeerRenames {
		if pending.seq > best {
			name, best = pending.name, pending.seq
		}
	}
	return name
}

// CrossSessionEnabled reports whether this process joined the machine-wide
// peer network at spawn.
//
// Spawn-time and immutable: the inbox binds during the CLI's setup and no
// control request rebinds it, which is why a settings change converges by
// restart. Callers use it as the CHEAP first gate — everything else about
// peer messaging (deriving a name, reading the thread row) is wasted work
// on a session that never joined.
func (s *Session) CrossSessionEnabled() bool {
	return s.crossSessionEnabled
}

// PeerSessionName reports the peer-visible name this session currently
// answers to.
//
// It is the name the CLI itself REPORTED, read back out of the `/rename`
// command's own output and sanitized through the same mirror as every other
// name here — not the name AO asked for. The two differ exactly where it
// matters: the CLI exposes no query for the registered name, and on a
// collision it registers a variant of its own choosing while reporting both.
// A rename the CLI completed without a reply this parser recognises leaves
// this value alone rather than moving it to an unconfirmed name.
//
// A rename still in flight is deliberately NOT reported here — see
// settlePeerRename. RenamePeerSession consults the pending renames itself
// (peerRenameTargetLocked, which compares REQUESTS) so neither an in-flight
// rename nor one the CLI answered under another name is re-sent on every turn
// boundary.
func (s *Session) PeerSessionName() string {
	s.peerNameMu.Lock()
	defer s.peerNameMu.Unlock()
	return s.peerSessionName
}

// ErrPeerRenameUnavailable is returned when the session cannot be renamed
// because it never joined the peer network. The caller treats it as
// "nothing to do", not as a failure: a session with no inbox has no
// peer-visible name to correct.
var ErrPeerRenameUnavailable = fmt.Errorf("claude: cross-session messaging is not enabled for this session")

// RenamePeerSession changes the name peers address this session by,
// WITHOUT restarting it.
//
// The mechanism is the CLI's own `/rename` slash command sent as an
// ordinary stdin user message, and the two properties that make it
// usable here were both measured rather than assumed (spike 2026-08-21,
// 2.1.237, /tmp/spike-xsession/logs/q7):
//
//   - It costs NOTHING. `/rename` is a LOCAL command: the result envelope
//     comes back `num_turns: 0` with the session's cumulative cost
//     unmoved, and no request reaches the model. The output is one
//     `<synthetic>` assistant line, "Session renamed to: X".
//   - It ACKS like any AO send. Because the envelope carries AO's
//     client-minted uuid, the CLI emits the full command_lifecycle
//     bracket AND the `<command-name>` replay echo preserving that uuid —
//     which is what lets triage's pending-send correlator consume the
//     send instead of stranding it. A stranded pending entry poisons turn
//     indexing for the rest of the session (incident 2026-08-04), so
//     minting the uuid here is not optional.
//
// The control-request sibling `rename_session` is deliberately NOT used:
// it moves the session TITLE only and leaves the peer registry alone
// (verified in the same spike), so it would report success while every
// peer kept addressing the old name.
//
// Refuses an empty sanitized name rather than sending it: the CLI answers
// an empty `/rename` with "That name is empty once invisible characters
// are removed" and changes nothing, which would be an error surfaced as a
// success here.
func (s *Session) RenamePeerSession(ctx context.Context, name string) error {
	if !s.crossSessionEnabled {
		return ErrPeerRenameUnavailable
	}
	sanitized := SanitizePeerSessionName(name)
	if sanitized == "" {
		return fmt.Errorf("claude: peer session name is empty after sanitization")
	}
	// Held across the dedup check, the staging, AND the write. Two
	// concurrent renames that only serialized their staging can still
	// interleave as stage A, stage+send B, send A — the CLI ends on A
	// while the cache says B, and because a rename whose wanted name
	// already matches is skipped, the session is then stuck under the
	// wrong peer name for its whole life. Serializing the write is what
	// makes "last to stage" and "last on the wire" the same rename.
	s.peerRenameSendMu.Lock()
	defer s.peerRenameSendMu.Unlock()

	s.peerNameMu.Lock()
	if sanitized == s.peerRenameTargetLocked() {
		// Already there, or already asked for and awaiting its ack. The
		// pending arm matters because the caller is a turn-boundary
		// reconcile: without it, every turn until the bracket lands would
		// send another identical `/rename`.
		s.peerNameMu.Unlock()
		return nil
	}
	id := uuid.NewString()
	s.peerRenameSeq++
	// Staged, not committed. A stdin write that Send accepts says only that
	// the bytes left AO; the registry moves when the CLI's own command
	// completes, and settlePeerRename is what promotes it. Registered before
	// the write because the terminal frame can reach the read loop before
	// Send returns.
	if s.pendingPeerRenames == nil {
		s.pendingPeerRenames = make(map[string]pendingPeerRename, 1)
	}
	s.pendingPeerRenames[id] = pendingPeerRename{name: sanitized, seq: s.peerRenameSeq}
	s.peerNameMu.Unlock()

	// Registered BEFORE the write, exactly like every other AO send: the
	// bracket can arrive on the read loop before Send returns, and an
	// unregistered uuid would classify this app's own rename as a peer
	// turn.
	s.noteIssuedCommandUUID(id)
	err := s.Send(ctx, "/rename "+sanitized, provider.SendOptions{
		UserMessageUUID: id,
		// Without this the outbound slash guard prefixes a newline and the
		// CLI's router never claims the line — the rename would arrive at
		// the model as prose (slash_guard.go).
		AllowClaudeSlashCommand: true,
		// AO's own bookkeeping: the user never typed this, so its
		// "Session renamed to: …" output must not land in their transcript.
		// Row only — the bracket still settles and the name still promotes
		// (command_result_suppression.go).
		InternalCommand: true,
	})
	if err != nil {
		s.issuedCommands.release(id)
		s.peerNameMu.Lock()
		delete(s.pendingPeerRenames, id)
		s.peerNameMu.Unlock()
		return fmt.Errorf("claude: rename peer session: %w", err)
	}
	return nil
}

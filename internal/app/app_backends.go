package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"

	"agent-overflow/internal/attachedbackends"
	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/store"
)

// The attached-backend admin surface: adding, naming and removing the
// OTHER machines this installation drives (docs/specs/remote-access.md
// §10).
//
// These methods use `host` scope, which makes them local-machine-only
// without a second rule: host presence is the only key that opens a host
// method, and no session grant does. That matters more here than almost
// anywhere else on the wire — a caller that could attach a backend could
// point this installation at a machine of its choosing, and a caller that
// could list them learns every computer this person works on.
//
// They use `home` route: they act on THIS backend's own profile
// directory. Asked of an attached backend they would answer about that
// machine's attachments, which is a real thing to want one day and not
// what this surface is.

// AttachedBackend is one attached machine. Aliased to the canonical shape
// so the wire type and the stored one cannot drift.
type AttachedBackend = attachedbackends.Attached

// BackendAttachment is what AddBackend answers.
type BackendAttachment = attachedbackends.Attachment

// BackendAttachOutcome is the frame the backend:attach channel carries:
// how one pairing ended, minutes after the RPC that started it returned.
type BackendAttachOutcome struct {
	ID string `json:"id"`
	// Attached is true when the owner of the far machine confirmed the
	// number. False means it ended some other way, and Error says how.
	Attached bool `json:"attached"`
	// Error is the reason, empty on success. Carried as text because
	// there is nothing for a client to branch on: every failure here ends
	// with the same remedy, which is to pair again.
	Error string `json:"error,omitempty"`
}

// BackendSetChange is the frame the backend:set-changed channel carries.
// The shape and its action vocabulary live in internal/attachedbackends,
// because the manager is the one emitter — every mutation of the set, the
// ones this surface asks for and the ones the reconciler and name
// synchronization make on their own, reaches the page through the
// observer SetAttachedBackends registers. The frontend-only desktop
// (internal/frontendclient) registers the same observer over the same
// type, so there is one wire shape and one TS mirror.
type BackendSetChange = attachedbackends.SetChange

// errNoBackendProfiles is what every method here answers when this boot
// keeps no device profile directory.
var errNoBackendProfiles = errors.New("this installation has nowhere to keep pairings, so it cannot attach to other machines")

// ListBackends returns every machine this installation has attached.
//
// Reachability is last-known and never probed: waking every attached
// laptop to answer one list would make the settings pane as slow as the
// slowest of them, and the SPA already learns the live answer from each
// carried socket.
//
//ao:scope host
//ao:route home
func (a *App) ListBackends() ([]AttachedBackend, error) {
	if a.backends == nil {
		return nil, errNoBackendProfiles
	}
	attached, err := a.backends.List()
	if err != nil {
		return nil, err
	}
	// Always a non-nil slice so the encoder emits `[]` rather than
	// `null`, and no caller needs a defensive coalesce.
	if attached == nil {
		attached = []AttachedBackend{}
	}
	return attached, nil
}

// AddBackend spends a pairing link and returns the verification number
// the owner of the far machine has to match.
//
// It returns BEFORE the pairing admits anything, and that is not a
// shortcut: the confirmation window is ten minutes
// (deviceclient.AwaitActivation), which is longer than any timeout
// between here and the page. So the wait runs on its own and the outcome
// arrives on the backend:attach channel — the same split the terminal
// ceremony makes between printing the number and waiting for it.
//
//ao:scope host
//ao:route home
//ao:remote
func (a *App) AddBackend(pairingLink string) (BackendAttachment, error) {
	if a.backends == nil {
		return BackendAttachment{}, errNoBackendProfiles
	}
	attachment, err := a.backends.Add(a.appCtx, pairingLink)
	if err != nil {
		return BackendAttachment{}, err
	}
	a.awaitAttachment(attachment.ID)
	return attachment, nil
}

// RepairBackendAddress verifies a new address using an existing pairing's
// certificate trust. It never enrolls another device or replaces its session.
//
//ao:scope host
//ao:route home
//ao:remote
func (a *App) RepairBackendAddress(ctx context.Context, id, endpoint string) (string, error) {
	if a.backends == nil {
		return "", errNoBackendProfiles
	}
	return a.backends.RepairAddress(ctx, id, endpoint)
}

// awaitAttachment runs one confirmation wait and announces how it ended.
//
// On the App's own context so a shutdown mid-wait ends it rather than
// leaving a goroutine holding a pinned TLS transport open for ten
// minutes.
func (a *App) awaitAttachment(id string) {
	go func() {
		outcome := BackendAttachOutcome{ID: id, Attached: true}
		if err := a.backends.Await(a.appCtx, id); err != nil {
			// Logged as well as announced: this is the one error in the
			// pairing flow nobody is standing in front of, and the page
			// that asked may be long closed.
			log.Printf("app: attach backend %s: %v", id, err)
			outcome.Attached = false
			outcome.Error = err.Error()
		}
		if outcome.Attached {
			a.backends.WakeOwnDevices()
		}
		a.emit(eventchan.BackendAttach, outcome)
		a.signalRemotePeers()
		a.emit(eventchan.AgentComputersChanged, struct{}{})
	}()
}

// RemoveBackend forgets one machine. The device key survives — it names
// this DEVICE, and the far side adopts its row again by thumbprint if
// this installation ever pairs with that machine a second time.
//
// `abandon` is the confirmation for what forgetting drops: the agent thread
// requests this computer still has open there, and the finished jobs whose
// completions are held for archived conversations, which can no longer be
// delivered. Without it they are named and the call refuses; with it the
// pairing goes, then its work ends with the wakes dropped and the held
// completions are discarded, all inside the fence admission takes, so no
// request enters it meanwhile. The other computer is not told, because
// forgetting it is exactly the decision to stop talking to it.
//
// Removing the profile is the commit point: a forget that fails before it
// leaves the pairing and every record for it as they were, and one whose
// ledger write fails after it leaves the records to the sweep, which ends
// the work of a computer with no pairing saved (endUnpaired).
//
//ao:scope host
//ao:route home
func (a *App) RemoveBackend(id string, abandon bool) error {
	if a.backends == nil {
		return errNoBackendProfiles
	}
	unlock := a.lockComputerPairing(id)
	defer unlock()
	held, other, stopped, err := a.store.PendingRemoteWatchesForComputer(id)
	if err != nil {
		return err
	}
	if other > 0 || stopped > 0 {
		return errors.New("This computer has remote commands awaiting completion or confirmation" + stoppedRemoteCommands(stopped) +
			". Stop them from their conversations and wait for confirmation before forgetting the computer. If it is offline, reconnect it first so cancellation remains available.")
	}
	if err := a.refuseForgettingOpenWork(id, held, abandon); err != nil {
		return err
	}
	// The name it goes by now, which names its work from before names
	// were kept once it is gone.
	profile, _, err := a.backends.Lookup(id)
	if err != nil {
		return err
	}
	if err := a.backends.Remove(id); err != nil {
		return err
	}
	a.releaseForgottenComputerWork(id, attachedComputerName(profile))
	return nil
}

// refuseForgettingOpenWork refuses once with what forgetting drops named,
// unless the caller confirmed it.
func (a *App) refuseForgettingOpenWork(id string, held int, abandon bool) error {
	if abandon {
		return nil
	}
	rows, open, err := a.store.ListOpenThreadRequestsForComputer(id, threadForgetRequestListing)
	if err != nil {
		return err
	}
	if open > 0 || held > 0 {
		// Public, so the listing reaches a connected browser too and so
		// the confirm affordance keys on the code rather than the prose.
		if open == 0 {
			return errorsx.Public("remote_completions_held", heldRemoteCompletions(held)+" Confirm to forget the computer anyway.", nil)
		}
		discarded := ""
		if held > 0 {
			discarded = " " + heldRemoteCompletions(held)
		}
		return errorsx.Public("thread_requests_open",
			describeOpenThreadRequests(rows, open)+
				". Forgetting it stops them from ever being collected."+discarded+" Confirm to forget it anyway; the other computer is not told and any work it started keeps running there.", nil)
	}
	return nil
}

// releaseForgottenComputerWork drops the work of a computer whose pairing
// the person just removed: every open thread request settles with its wake
// disarmed, every command there not known to have finished is recorded with
// an unknown outcome, the held completions are discarded, and its records
// that kept no name take name. The caller holds the computer's fence. The
// pairing is gone already, so a write that fails here is logged and left
// to the sweep, which ends the work of a computer with no pairing saved.
func (a *App) releaseForgottenComputerWork(id, name string) {
	// Disarmed: the person chose to stop hearing from that computer, so
	// no answer that can no longer arrive is told as an error either.
	if _, err := a.endPairingFenced(id, store.PairingEnd{
		RequestAnswer: []byte("This computer was forgotten, so this request can no longer be collected. The other computer was not told, and any work it started keeps running there."),
		CommandError:  "This computer was forgotten, so this command's outcome is unknown.",
		Disarm:        true,
		ComputerName:  name,
	}); err != nil {
		log.Printf("app: forget %s: end its work, which the sweep ends instead: %v", id, err)
	}
	threads, err := a.store.DismissArchivedRemoteCompletions(id)
	if err != nil {
		log.Printf("app: forget %s: discard the completions held for archived conversations: %v", id, err)
	}
	for _, threadID := range threads {
		a.emitBackgroundChanged(threadID)
	}
}

// stoppedRemoteCommands names the stopped commands there whose cancel is
// still owed, which a forget would leave running with nobody to stop them.
func stoppedRemoteCommands(n int) string {
	switch n {
	case 0:
		return ""
	case 1:
		return ", including 1 stopped command whose cancellation it has not confirmed"
	}
	return fmt.Sprintf(", including %d stopped commands whose cancellation it has not confirmed", n)
}

// heldRemoteCompletions names the completions forgetting a computer discards.
func heldRemoteCompletions(n int) string {
	if n == 1 {
		return "1 finished job held for an archived conversation will be discarded."
	}
	return fmt.Sprintf("%d finished jobs held for archived conversations will be discarded.", n)
}

// threadForgetRequestListing bounds the refusal's list. More open requests
// than this against one computer is a number, not a list.
const threadForgetRequestListing = 20

// describeOpenThreadRequests names the open requests forgetting a computer
// abandons: how many there are, and the first of them.
func describeOpenThreadRequests(rows []store.ThreadRequest, total int) string {
	parts := make([]string, 0, len(rows))
	for _, row := range rows {
		part := row.Kind + " " + row.Token
		if row.TargetThreadID != "" {
			part += " on thread " + row.TargetThreadID
		}
		parts = append(parts, part)
	}
	count := "1 agent thread request"
	if total != 1 {
		count = fmt.Sprintf("%d agent thread requests", total)
	}
	listed := strings.Join(parts, ", ")
	if more := total - len(rows); more > 0 {
		listed += fmt.Sprintf(", and %d more", more)
	}
	return "This computer has " + count + " still open there: " + listed
}

// RenameBackend sets what this installation calls one machine, or clears
// it when the nickname is empty. Local: nothing is sent to the machine
// being renamed, which goes on calling itself whatever it calls itself.
//
//ao:scope host
//ao:route home
func (a *App) RenameBackend(id, nickname string) error {
	if a.backends == nil {
		return errNoBackendProfiles
	}
	return a.backends.Rename(id, nickname)
}

// AttachedBackends exposes the manager to the transport, which serves one
// carried route family per attached machine. Package-level for the same
// reason BackendIdentity is: it is boot wiring, not a bound method.
//
// Returns a nil interface rather than a typed nil when this boot keeps no
// profiles, so the transport registers no routes at all.
func AttachedBackends(a *App) *attachedbackends.Manager { return a.backends }

// SetAttachedBackends installs the manager during boot.
func SetAttachedBackends(a *App, manager *attachedbackends.Manager) {
	a.backends = manager
	if manager != nil {
		manager.SetNetwork(func() string { id, _ := a.backendIdentity(); return id }, a.dialComputer)
		if a.netReach.LoopbackOnly() {
			manager.DisableLANDiscovery()
		}
		manager.SetChanged(a.backendSetChanged)
	}
}

// backendSetChanged is the attached-machine set's one observer: a removed
// machine's pairing ends here (endPairing) and its Agents rows are
// forgotten, then every client is told.
func (a *App) backendSetChanged(change attachedbackends.SetChange) {
	switch change.Action {
	case attachedbackends.SetRemoved:
		if change.Reason == attachedbackends.RemovedByComputer {
			a.endRevokedPairing(change)
		}
		a.forgetPeerAgentRows(change.ID)
	case attachedbackends.SetMembership:
		a.forgetUnpairedPeerAgentRows()
	}
	a.emit(eventchan.BackendSetChanged, change)
	a.signalRemotePeers()
	a.emit(eventchan.AgentComputersChanged, struct{}{})
}

package app

import (
	"context"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"agent-overflow/internal/network"
	"agent-overflow/internal/settings"
	"agent-overflow/internal/slicesx"
	"agent-overflow/internal/tailnet"
	"agent-overflow/internal/transport"
)

// The tailnet node: whether this backend joins the owner's tailnet, and
// what the settings screen is told about it (docs/specs/remote-access.md
// §7, "Anywhere access").
//
// Same shape as the certificate reconciler beside it, for the same
// reason. One goroutine reconciles the persisted preference against the
// live node, attaches the node's listeners to the transport that is
// already running, and publishes a status the screen reads back. Boot and
// every settings write KICK it; neither does node work inline.
//
// Why not inline: bringing a node up ends in an interactive sign-in the
// first time — the owner has to open a link and approve the machine —
// which outlives every RPC timeout the frontend has. The status carries
// that link instead, and the loop attaches the listeners the moment the
// node reports it joined.
//
// What this file does NOT own: the wire. The listeners it attaches are
// answered by the transport's own mux, credentials, origin rules, Host
// guard and per-RPC scope gate, and by the one session store every other
// listener consults. Reaching this backend over the tailnet authorizes
// nothing on its own — it is a way IN, and the way in has never been the
// thing that decides what a caller may do.

const (
	// tailnetIdleInterval is the backstop cadence. The loop's real wake-ups
	// are the settings kick and the node's own status channel, so this only
	// covers a change neither of those announced.
	tailnetIdleInterval = time.Hour

	// tailnetRetryFloor and tailnetRetryCeiling bound the backoff after a
	// failed bring-up or attach. A control server that is unreachable now
	// is usually unreachable in thirty seconds too, and the node keeps its
	// own retry schedule underneath ours.
	tailnetRetryFloor   = 30 * time.Second
	tailnetRetryCeiling = 10 * time.Minute
)

// tailnetState is everything the loop and the status read share. Its zero
// value is an App with no loop running and no node, which is what every
// fixture that builds *App directly gets — and what every install that
// never turns the feature on runs as forever.
type tailnetState struct {
	mu sync.Mutex

	// lifecycle serializes node transitions across goroutines: the
	// reconciler holds it for a whole pass, and ForgetTailnetNode holds
	// it across stop-and-delete. Without it, a forget that checked the
	// setting could interleave with a bring-up still running from a pass
	// that read the setting's previous value, and the deletion would
	// race the files that bring-up is writing. Separate from mu, which
	// guards field access only and is never held across a node call.
	lifecycle sync.Mutex

	// dir is the config root the node's state directory lives under.
	// Empty means the loop never started.
	dir string

	// kick wakes the loop. Buffered depth 1: a second kick while one is
	// pending is the same request.
	kick chan struct{}

	// node is the live node, nil while the feature is off or a bring-up
	// failed. Single use — a restart builds a new one on the same
	// directory, which is how the node keeps its identity.
	node *tailnet.Node

	// controlURL is the coordination server the live node was STARTED
	// with, which is not always what settings now say. The difference is
	// what makes a control-URL edit a restart rather than a no-op.
	controlURL string

	// plain and secure are the attached listeners. secure is nil whenever
	// the tailnet has HTTPS turned off in its admin panel, which is a
	// tailnet setting no code here can substitute for.
	plain  *transport.AuxListener
	secure *transport.AuxListener

	// lastErr is this layer's own failure — a bring-up that did not
	// happen, a listener that stopped accepting — verbatim and cleared by
	// the next success. The NODE's own errors are read off its status, so
	// the two cannot overwrite each other.
	lastErr string

	// failures counts consecutive failed passes, for the backoff.
	failures int
}

// startTailnetLoop launches the reconciler. Called once from Start with
// the config root. A second call is a no-op, so a fixture that runs Start
// twice cannot fan out goroutines.
func (a *App) startTailnetLoop(dir string) {
	a.tailnet.mu.Lock()
	if a.tailnet.dir != "" {
		a.tailnet.mu.Unlock()
		return
	}
	a.tailnet.dir = dir
	a.tailnet.kick = make(chan struct{}, 1)
	kick := a.tailnet.kick
	a.tailnet.mu.Unlock()

	ctx := a.lifeCtx()
	go func() {
		for {
			wait := a.reconcileTailnet()
			// Re-read the channel every pass: the node it belongs to is
			// built and closed by this same goroutine, and a channel from
			// a node that has been closed is a closed channel, which would
			// spin.
			events := a.tailnetEvents()
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				a.stopTailnetNode()
				return
			case <-kick:
				timer.Stop()
			case <-events:
				timer.Stop()
			case <-timer.C:
			}
		}
	}()
}

// kickTailnet asks the loop to reconcile now. Never blocks and never
// fails: a kick with no loop running is a no-op, which is the state of
// every App built without Start.
func (a *App) kickTailnet() bool {
	a.tailnet.mu.Lock()
	kick := a.tailnet.kick
	a.tailnet.mu.Unlock()
	if kick == nil {
		return false
	}
	select {
	case kick <- struct{}{}:
	default:
	}
	return true
}

// tailnetEvents is the live node's status channel, or nil when there is
// no node. A nil channel blocks forever in a select, which is exactly the
// "nothing to hear from" case.
func (a *App) tailnetEvents() <-chan struct{} {
	a.tailnet.mu.Lock()
	defer a.tailnet.mu.Unlock()
	if a.tailnet.node == nil {
		return nil
	}
	return a.tailnet.node.Events()
}

// reconcileTailnet makes the live node match the persisted preference and
// answers how long to wait before looking again.
func (a *App) reconcileTailnet() time.Duration {
	a.tailnet.lifecycle.Lock()
	defer a.tailnet.lifecycle.Unlock()

	cfg := a.currentSettings().Network

	a.tailnet.mu.Lock()
	node := a.tailnet.node
	liveControl := a.tailnet.controlURL
	a.tailnet.mu.Unlock()

	if !cfg.WantsTailnet() {
		if node != nil {
			// The state directory is deliberately kept. Turning the
			// feature off is not the same act as leaving the tailnet, and
			// a user who toggles it back on expects the same device rather
			// than a second entry in their admin panel. ForgetTailnetNode
			// is the other act.
			a.stopTailnetNode()
		}
		a.clearTailnetFailure()
		return tailnetIdleInterval
	}

	if node != nil && liveControl != cfg.TailnetControlURL {
		// The coordination server changed under a running node. A node
		// cannot re-register elsewhere in place, so this is a stop and a
		// fresh start on the same identity — which the new control server
		// will not recognize, so the status goes back to waiting for a
		// sign-in and says so.
		log.Printf("tailnet: coordination server changed, restarting the node")
		a.stopTailnetNode()
		node = nil
	}

	if node == nil {
		started, err := a.startTailnetNode(cfg)
		if err != nil {
			a.recordTailnetFailure(err.Error())
			return a.tailnetRetryDelay()
		}
		node = started
	}

	if err := a.attachTailnetListeners(node); err != nil {
		a.recordTailnetFailure(err.Error())
		return a.tailnetRetryDelay()
	}
	a.clearTailnetFailure()
	return tailnetIdleInterval
}

// startTailnetNode constructs and starts the node. It returns as soon as
// the node exists; reaching the tailnet is what the status reports.
func (a *App) startTailnetNode(cfg settings.NetworkSettings) (*tailnet.Node, error) {
	root := a.tailnetDir()
	if root == "" {
		return nil, fmt.Errorf("no configuration directory, so there is nowhere to keep this node's identity")
	}
	node, err := tailnet.New(tailnet.Options{
		Dir:        tailnet.StateDir(root),
		ControlURL: cfg.TailnetControlURL,
	})
	if err != nil {
		return nil, err
	}
	if err := node.Start(); err != nil {
		return nil, err
	}

	a.tailnet.mu.Lock()
	a.tailnet.node = node
	a.tailnet.controlURL = cfg.TailnetControlURL
	a.tailnet.mu.Unlock()
	return node, nil
}

// attachTailnetListeners gives the transport the node's listeners once it
// has joined, and tells the Host guard the names those listeners answer
// to. A node that has not joined yet is not an error — it is waiting for
// the owner, and the loop is woken by the node's own status channel when
// that changes.
func (a *App) attachTailnetListeners(node *tailnet.Node) error {
	status := node.Status()
	if !status.Running() {
		return nil
	}
	srv := a.transportServer.Load()
	if srv == nil {
		return fmt.Errorf("the transport server is not running, so there is nothing to serve on the tailnet")
	}

	a.tailnet.mu.Lock()
	havePlain := a.tailnet.plain != nil
	haveSecure := a.tailnet.secure != nil
	a.tailnet.mu.Unlock()

	if !havePlain {
		// The SAME numeric port the main listener binds. Netstack ports
		// are userspace, so there is no conflict and no privilege
		// question — and one port across every way in keeps the cookie
		// name, the origin derivation and the share URL uniform.
		ln, err := node.Listen(portFromAddr(srv.Addr()))
		if err != nil {
			return err
		}
		aux, err := srv.ServeAuxiliary(ln, a.tailnetListenerFailed)
		if err != nil {
			_ = ln.Close()
			return err
		}
		a.tailnet.mu.Lock()
		a.tailnet.plain = aux
		a.tailnet.mu.Unlock()
	}

	// The Host guard admits these names while the node is up, and only
	// while it is up. Set after the listener exists so a name is never
	// admitted before something answers on it, and cleared by
	// stopTailnetNode so one is never admitted after.
	srv.SetAuxiliaryHosts(append(append([]string(nil), status.IPs...), status.DNSName))

	if haveSecure || len(status.CertDomains) == 0 {
		// No certificate names means the tailnet has HTTPS turned off in
		// its admin panel. Cleartext over WireGuard is the honest answer,
		// and the status says which one the user is getting.
		return nil
	}
	ln, err := node.ListenTLS()
	if err != nil {
		// Report it and keep the cleartext listener: losing HTTPS is a
		// degraded tailnet, not an unreachable one.
		return fmt.Errorf("serve HTTPS on the tailnet: %w", err)
	}
	aux, err := srv.ServeAuxiliary(ln, a.tailnetListenerFailed)
	if err != nil {
		_ = ln.Close()
		return err
	}
	a.tailnet.mu.Lock()
	a.tailnet.secure = aux
	a.tailnet.mu.Unlock()
	return nil
}

// tailnetListenerFailed is the auxiliary accept loop's terminal error.
// Recorded as user-facing state and kicked, so the next pass re-attaches
// rather than leaving a node that is up with nothing listening on it.
//
// It reaches only this feature: the transport reports an auxiliary
// listener's failure to the caller that handed it over, never on the
// shared serve-error channel, so a tailnet that stopped accepting can
// never read as the app's own transport dying.
func (a *App) tailnetListenerFailed(err error) {
	a.dropTailnetListeners()
	a.recordTailnetFailure(fmt.Sprintf("the tailnet listener stopped accepting: %v", err))
	a.kickTailnet()
}

// stopTailnetNode detaches the listeners and closes the node. Safe on a
// state with neither.
func (a *App) stopTailnetNode() {
	a.dropTailnetListeners()

	a.tailnet.mu.Lock()
	node := a.tailnet.node
	a.tailnet.node = nil
	a.tailnet.controlURL = ""
	a.tailnet.mu.Unlock()

	if node == nil {
		return
	}
	if err := node.Close(); err != nil {
		log.Printf("tailnet: %v", err)
	}
}

// dropTailnetListeners detaches whatever is attached and withdraws the
// names the Host guard was admitting for it. A name that stays admitted
// after the listener behind it is gone is an admission nobody can reach.
func (a *App) dropTailnetListeners() {
	a.tailnet.mu.Lock()
	plain, secure := a.tailnet.plain, a.tailnet.secure
	a.tailnet.plain, a.tailnet.secure = nil, nil
	a.tailnet.mu.Unlock()

	for _, aux := range []*transport.AuxListener{plain, secure} {
		if aux == nil {
			continue
		}
		if err := aux.Close(); err != nil {
			log.Printf("tailnet: detach listener: %v", err)
		}
	}
	if srv := a.transportServer.Load(); srv != nil {
		srv.SetAuxiliaryHosts(nil)
	}
}

func (a *App) recordTailnetFailure(message string) {
	log.Printf("tailnet: %s", message)
	a.tailnet.mu.Lock()
	defer a.tailnet.mu.Unlock()
	a.tailnet.lastErr = message
	a.tailnet.failures++
}

func (a *App) clearTailnetFailure() {
	a.tailnet.mu.Lock()
	defer a.tailnet.mu.Unlock()
	a.tailnet.lastErr = ""
	a.tailnet.failures = 0
}

func (a *App) tailnetDir() string {
	a.tailnet.mu.Lock()
	defer a.tailnet.mu.Unlock()
	return a.tailnet.dir
}

// tailnetRetryDelay doubles from the floor to the ceiling with the
// consecutive-failure count.
func (a *App) tailnetRetryDelay() time.Duration {
	a.tailnet.mu.Lock()
	failures := a.tailnet.failures
	a.tailnet.mu.Unlock()
	delay := tailnetRetryFloor
	for i := 1; i < failures && delay < tailnetRetryCeiling; i++ {
		delay *= 2
	}
	if delay > tailnetRetryCeiling {
		delay = tailnetRetryCeiling
	}
	return delay
}

// tailnetStatus is the read-only half the settings screen shows.
func (a *App) tailnetStatus() network.TailnetStatus {
	a.tailnet.mu.Lock()
	node := a.tailnet.node
	dir := a.tailnet.dir
	status := network.TailnetStatus{
		HTTPS:     a.tailnet.secure != nil,
		LastError: a.tailnet.lastErr,
	}
	a.tailnet.mu.Unlock()

	status.HasState = tailnetStateExists(dir)
	if node != nil {
		observed := node.Status()
		status.State = observed.State
		status.Running = observed.Running()
		status.AuthURL = observed.AuthURL
		status.DNSName = observed.DNSName
		status.IPs = observed.IPs
		if status.LastError == "" {
			status.LastError = observed.LastErr
		}
	}
	// `[]`, never `null`: the field is a list the screen renders, and a
	// client should not have to coalesce one absent value per read.
	status.IPs = slicesx.OrEmpty(status.IPs)
	return status
}

// tailnetStateExists reports whether a node identity is on disk, which is
// what makes "forget this node" offerable only when there is something to
// forget. One stat, and only on a settings read.
func tailnetStateExists(configRoot string) bool {
	if configRoot == "" {
		return false
	}
	info, err := os.Stat(tailnet.StateDir(configRoot))
	return err == nil && info.IsDir()
}

// ForgetTailnetNode deletes this backend's tailnet identity from disk, so
// the next enable enrolls a fresh device. It is how the owner moves the
// backend to a different tailnet or a different account: the node key and
// the machine key live in that one directory and there is no other way to
// change which tailnet they belong to.
//
// Refused while the feature is enabled. Deleting the state under a live
// node would leave this process holding an identity nothing on disk
// records, and the owner's admin panel would keep showing a device that
// no longer has a way back.
//
// `host` scope and deliberately NO step-up. Every act this call can
// perform is a DELETION of local state: it cannot widen what this backend
// exposes, it cannot enroll anything, and it cannot run at all until the
// feature has already been turned off — which is itself a step-up-gated
// write through SetNetworkSettings. Demanding a second proof would gate
// the cleanup after an act that was already proved, which is the same
// argument RenewCanonicalDomainCert makes.
//
//ao:scope host
//ao:route home
func (a *App) ForgetTailnetNode(ctx context.Context) (network.Settings, error) {
	if a.settings == nil {
		return network.Settings{}, fmt.Errorf("settings service unavailable")
	}
	if a.currentSettings().Network.WantsTailnet() {
		return network.Settings{}, fmt.Errorf(
			"turn tailnet access off before forgetting this node, so it stops before its identity is removed")
	}
	root := a.tailnetDir()
	if root == "" {
		return network.Settings{}, fmt.Errorf("no configuration directory, so there is no node state to remove")
	}
	// The lifecycle lock keeps this from interleaving with a reconcile
	// pass that is still bringing a node up from the setting's previous
	// value — without it, the deletion below could race the files that
	// bring-up is writing, and the forgotten directory would reappear
	// holding a fresh identity.
	a.tailnet.lifecycle.Lock()
	defer a.tailnet.lifecycle.Unlock()
	// Belt and braces against a node that outlived a disable: closing an
	// already-closed node is a no-op, and deleting state under a live one
	// is the failure this refuses.
	a.stopTailnetNode()
	if err := tailnet.Forget(root); err != nil {
		return network.Settings{}, err
	}
	a.clearTailnetFailure()
	return a.networkSettingsForCaller(ctx, a.persistedNetworkSettings()), nil
}

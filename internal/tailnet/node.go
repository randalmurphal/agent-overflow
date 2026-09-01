package tailnet

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"tailscale.com/client/local"
	"tailscale.com/envknob"
	"tailscale.com/ipn"
	"tailscale.com/tsnet"
	"tailscale.com/types/logger"
)

const (
	// DirName is the state directory this package keeps under the app's
	// config root. Named rather than spelled at call sites because Forget
	// deletes it and StateDir builds it, and a second spelling is a
	// deletion pointed somewhere else.
	DirName = "tsnet"

	// DefaultHostname is the name this node presents to the coordination
	// server. Not configurable: a tailnet node's name is how the owner
	// finds it in their device list, and one install is one node.
	DefaultHostname = "agent-overflow"

	// HTTPSPort is the port the tailnet HTTPS listener answers on. Fixed
	// at 443 because the whole value of the ts.net certificate is that a
	// browser opens https://<node>.<tailnet>.ts.net with no port and no
	// warning.
	HTTPSPort = 443

	// watchRetryDelay is how long the status watcher waits before
	// re-subscribing to the IPN bus after the subscription ended while
	// the node is still alive. Short, because until it is back the
	// published status is frozen at whatever it last saw.
	watchRetryDelay = time.Second

	// closeDrainTimeout bounds how long Close waits for the node's
	// goroutines to return. tsnet.Close is synchronous for the parts it
	// can be, but its teardown fans out ~45 goroutines and one spike run
	// in 27 cycles had stragglers past 30s — so the wait is bounded and
	// a timeout is reported rather than hung on.
	closeDrainTimeout = 15 * time.Second
)

// StateDir is where a node rooted at the app's config root keeps its
// identity.
//
// AT REST THIS DIRECTORY IS THE NODE. tsnet writes `tailscaled.state`
// (which holds the private node key inside its persisted prefs blob) and
// `tailscaled.log.conf` (which holds a private logging id), chmods the
// directory 0700 and the files 0600 itself, and rebuilds the same node
// identity from them on every later start. Possession of these bytes is
// possession of this backend's place on the owner's tailnet, so anything
// that copies, backs up or serves the config root has to treat them the
// way it treats the session signing key.
func StateDir(configRoot string) string {
	return filepath.Join(configRoot, DirName)
}

// Options configure a node. Everything here is fixed for the node's
// life; changing one means closing this node and starting another,
// which is what internal/app's reconciler does.
type Options struct {
	// Dir is the state directory, normally StateDir(configRoot).
	// Required: without somewhere to keep the node key, every start
	// would enroll a new device in the owner's tailnet.
	Dir string

	// Hostname is the name presented to the coordination server. Empty
	// means DefaultHostname.
	Hostname string

	// ControlURL is the coordination server. Empty means tsnet's own
	// default, which is the Tailscale service; a self-hosted control
	// plane (Headscale) is the reason this is configurable at all.
	ControlURL string

	// Logf receives the backend's (very verbose) logs. Nil discards
	// them, which is what production passes: the node's user-facing
	// state is Status, not a log stream.
	Logf logger.Logf
}

// Status is what the node currently is, as one immutable snapshot.
// Errors are user-facing state (root AGENTS.md principle 5), so LastErr
// is carried here rather than only logged.
type Status struct {
	// State is the tailscale backend state — "NeedsLogin", "Starting",
	// "Running", "Stopped". Empty before the node is started.
	State string

	// AuthURL is the sign-in link to open while the node is waiting for
	// the owner to approve it, and empty otherwise.
	AuthURL string

	// DNSName is the node's MagicDNS name with no trailing dot, empty
	// until the node is Running.
	DNSName string

	// IPs are the node's tailnet addresses.
	IPs []string

	// CertDomains are the names tsnet can obtain a certificate for.
	// Empty means the tailnet has HTTPS turned off in its admin panel,
	// which no code here can substitute for.
	CertDomains []string

	// LastErr is the most recent failure, verbatim, cleared by the next
	// success.
	LastErr string
}

// Running reports whether the node is fully up.
func (s Status) Running() bool { return s.State == ipn.Running.String() }

// Node is one tsnet node: construct, start, hand out listeners, close.
//
// SINGLE USE. A closed Node is not restarted — the caller builds
// another one on the same Dir, which is exactly how the node keeps its
// identity across an off/on cycle. Making Node itself restartable would
// mean carrying a started flag through two more states for no gain, and
// the one operation that MUST consult that flag (Close, which panics on
// a tsnet.Server that was never started) is easier to get right when it
// is asked once.
type Node struct {
	opts Options

	// mu guards everything below. Held only for field reads and writes,
	// never across a tsnet call.
	mu      sync.Mutex
	srv     *tsnet.Server
	lc      *local.Client
	started bool
	closed  bool
	status  Status

	// events carries one wake-up per status change, coalesced. The
	// reconciler selects on it so attaching a listener the moment the
	// node reaches Running needs no poll.
	events chan struct{}

	// stopWatch ends the status watcher; watchDone closes when it has
	// returned.
	stopWatch context.CancelFunc
	watchDone chan struct{}
}

// New builds a node. Nothing happens yet: no goroutine, no socket, no
// file. A user who never turns the feature on pays the cost of this
// struct and nothing else (spec §7, "tsnet is opt-in and lazily
// initialized").
func New(opts Options) (*Node, error) {
	if strings.TrimSpace(opts.Dir) == "" {
		return nil, fmt.Errorf("tailnet: a state directory is required, or every start would enroll a new device")
	}
	if opts.Hostname == "" {
		opts.Hostname = DefaultHostname
	}
	return &Node{
		opts:      opts,
		events:    make(chan struct{}, 1),
		watchDone: make(chan struct{}),
	}, nil
}

// Events delivers one wake-up per status change, coalesced to a depth of
// one. Never closed while the node lives; Close closes it, so a
// reconciler selecting on it also learns the node went away.
func (n *Node) Events() <-chan struct{} { return n.events }

// Start brings the node up and begins publishing status. It returns as
// soon as the backend is constructed — reaching Running takes an
// interactive sign-in the first time, and the caller watches Status for
// that rather than blocking here.
//
// Deliberately NOT built on tsnet's Up: Up waits for Running and never
// returns while the node sits in NeedsLogin, so an app that called it
// would have no way to show the owner the link that would end the wait.
func (n *Node) Start() error {
	n.mu.Lock()
	if n.closed {
		n.mu.Unlock()
		return fmt.Errorf("tailnet: node is closed")
	}
	if n.started {
		n.mu.Unlock()
		return nil
	}
	n.mu.Unlock()

	if err := os.MkdirAll(n.opts.Dir, 0o700); err != nil {
		return fmt.Errorf("tailnet: create state directory: %w", err)
	}

	// Opt out of the log upload, always and unconditionally. tsnet
	// otherwise streams backend logs to log.tailscale.com for support
	// purposes; this feature's whole posture is that nothing leaves a
	// path the owner controls (spec §7), so the upload is not a choice
	// we offer and there is no setting for it. Set before Start because
	// the logger is built during it.
	noLogsOnce.Do(envknob.SetNoLogsNoSupport)

	logf := n.opts.Logf
	if logf == nil {
		logf = logger.Discard
	}
	srv := &tsnet.Server{
		Dir:        n.opts.Dir,
		Hostname:   n.opts.Hostname,
		ControlURL: n.opts.ControlURL,
		Logf:       logf,
		// The user-facing half of tsnet's own logging is the "go to this
		// URL" line, which this package publishes as Status.AuthURL
		// instead. Printing it to the app log as well would put a
		// single-use sign-in link somewhere it outlives its use.
		UserLogf: logger.Discard,
	}
	if err := srv.Start(); err != nil {
		return fmt.Errorf("tailnet: start node: %w", err)
	}
	lc, err := srv.LocalClient()
	if err != nil {
		// The node started, so it must be closed rather than leaked —
		// and it IS started, so Close is safe to call.
		_ = srv.Close()
		return fmt.Errorf("tailnet: reach the node's local API: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	n.mu.Lock()
	n.srv = srv
	n.lc = lc
	n.started = true
	n.stopWatch = cancel
	n.mu.Unlock()

	go n.watch(ctx, lc)
	return nil
}

// noLogsOnce keeps the process-global opt-out to one call. The knob is a
// process property, not a per-node one.
var noLogsOnce sync.Once

// Listen returns a cleartext listener on the tailnet for the given port.
// The caller owns it: closing the listener stops serving, closing the
// node stops everything.
//
// The port is the caller's, and internal/app passes the SAME port the
// main listener binds, so every URL, cookie name and origin derivation
// stays uniform across the two. Netstack ports are userspace, so there
// is no privilege question and no conflict with the host's own ports.
func (n *Node) Listen(port int) (net.Listener, error) {
	srv, err := n.runningServer()
	if err != nil {
		return nil, err
	}
	ln, err := srv.Listen("tcp", ":"+strconv.Itoa(port))
	if err != nil {
		return nil, fmt.Errorf("tailnet: listen on port %d: %w", port, err)
	}
	return ln, nil
}

// ListenTLS returns an HTTPS listener on the tailnet's HTTPS port,
// serving a certificate tsnet obtains for the node's own ts.net name.
//
// tsnet owns that certificate end to end — it resolves one per handshake
// and orders it live on the first one — so nothing in this repository
// mints, stores or renews it, and the transport's own certificate source
// is not involved. It refuses unless the tailnet has MagicDNS and HTTPS
// enabled in its admin panel, which is a tailnet setting no code here
// can substitute for; the caller reports that refusal as status and
// serves the cleartext listener alone.
func (n *Node) ListenTLS() (net.Listener, error) {
	srv, err := n.runningServer()
	if err != nil {
		return nil, err
	}
	ln, err := srv.ListenTLS("tcp", ":"+strconv.Itoa(HTTPSPort))
	if err != nil {
		return nil, fmt.Errorf("tailnet: listen for HTTPS: %w", err)
	}
	return ln, nil
}

// runningServer answers the started node, refusing a node that is not up
// yet. Both listen calls need it, and tsnet's own ListenTLS would
// otherwise block forever waiting for a sign-in that has not happened.
func (n *Node) runningServer() (*tsnet.Server, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	switch {
	case n.closed:
		return nil, fmt.Errorf("tailnet: node is closed")
	case !n.started:
		return nil, fmt.Errorf("tailnet: node is not started")
	case !n.status.Running():
		return nil, fmt.Errorf("tailnet: node is not on the tailnet yet (%s)", n.stateForMessage())
	}
	return n.srv, nil
}

// stateForMessage renders the backend state for an error sentence.
// Called under mu.
func (n *Node) stateForMessage() string {
	if n.status.State == "" {
		return "starting"
	}
	return n.status.State
}

// Status returns the current snapshot. Safe on a node that was never
// started, which answers a zero Status.
func (n *Node) Status() Status {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.status.clone()
}

// Close stops the node and waits, bounded, for its goroutines. Safe to
// call twice, and safe to call on a node that was never started.
//
// The never-started case is the one that must be guarded rather than
// tried: tsnet.Server.Close on a Server that has not run Start
// dereferences a nil backend and panics, and a disable that arrives
// while an enable is still failing is exactly that shape.
func (n *Node) Close() error {
	n.mu.Lock()
	if n.closed {
		n.mu.Unlock()
		return nil
	}
	n.closed = true
	srv, started, stop := n.srv, n.started, n.stopWatch
	n.srv, n.lc, n.stopWatch = nil, nil, nil
	close(n.events)
	n.mu.Unlock()

	if stop != nil {
		stop()
		<-n.watchDone
	}
	if !started || srv == nil {
		return nil
	}

	done := make(chan error, 1)
	go func() { done <- srv.Close() }()
	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("tailnet: close node: %w", err)
		}
		return nil
	case <-time.After(closeDrainTimeout):
		// The node's teardown is still running. Reporting rather than
		// waiting forever is the choice: the caller is a reconciler that
		// has to answer the user, and the stragglers are the node's own
		// goroutines, which hold no lock this process needs.
		return fmt.Errorf("tailnet: the node did not finish shutting down within %s", closeDrainTimeout)
	}
}

// watch owns the published status for the node's whole life. It
// re-subscribes if the bus ends while the node is still alive, because
// the alternative is a status frozen at whatever it last saw with
// nothing saying so.
func (n *Node) watch(ctx context.Context, lc *local.Client) {
	defer close(n.watchDone)
	for ctx.Err() == nil {
		err := n.watchOnce(ctx, lc)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			n.recordError(fmt.Sprintf("lost the connection to the node's status feed: %v", err))
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(watchRetryDelay):
		}
	}
}

// watchOnce holds one IPN bus subscription until it ends.
func (n *Node) watchOnce(ctx context.Context, lc *local.Client) error {
	watcher, err := lc.WatchIPNBus(ctx, ipn.NotifyInitialState)
	if err != nil {
		return err
	}
	defer watcher.Close()

	// The subscription is live, so whatever ended the last one is over.
	n.clearError()
	for {
		notify, err := watcher.Next()
		if err != nil {
			return err
		}
		n.apply(ctx, lc, notify)
	}
}

// apply folds one notification into the published status.
func (n *Node) apply(ctx context.Context, lc *local.Client, notify ipn.Notify) {
	changed := false

	n.mu.Lock()
	if notify.BrowseToURL != nil && n.status.AuthURL != *notify.BrowseToURL {
		n.status.AuthURL = *notify.BrowseToURL
		changed = true
	}
	stateMoved := false
	if notify.State != nil && n.status.State != notify.State.String() {
		n.status.State = notify.State.String()
		stateMoved = true
		changed = true
		if *notify.State == ipn.Running {
			// The link is spent. Leaving it published would offer the
			// owner a sign-in that is already done.
			n.status.AuthURL = ""
		}
	}
	n.mu.Unlock()

	if stateMoved {
		n.refreshIdentity(ctx, lc)
	}
	if changed {
		n.signal()
	}
}

// refreshIdentity re-reads the facts that only a full status carries:
// the MagicDNS name, the tailnet addresses, and the names a certificate
// could be obtained for. Read on a state change rather than per
// notification — the values move when the node joins, and a poll per
// heartbeat would be one local API round trip for an unchanged answer.
func (n *Node) refreshIdentity(ctx context.Context, lc *local.Client) {
	status, err := lc.StatusWithoutPeers(ctx)
	if err != nil || status == nil {
		return
	}
	name := ""
	var ips []string
	if status.Self != nil {
		name = strings.TrimSuffix(status.Self.DNSName, ".")
		for _, ip := range status.Self.TailscaleIPs {
			ips = append(ips, ip.String())
		}
	}
	domains := append([]string(nil), status.CertDomains...)

	n.mu.Lock()
	n.status.DNSName = name
	n.status.IPs = ips
	n.status.CertDomains = domains
	n.mu.Unlock()
}

func (n *Node) recordError(message string) {
	n.mu.Lock()
	changed := n.status.LastErr != message
	n.status.LastErr = message
	n.mu.Unlock()
	if changed {
		n.signal()
	}
}

func (n *Node) clearError() {
	n.mu.Lock()
	changed := n.status.LastErr != ""
	n.status.LastErr = ""
	n.mu.Unlock()
	if changed {
		n.signal()
	}
}

// signal wakes one waiter. Coalescing rather than queueing: a reader
// always reads the whole current status, so two changes that arrive
// before it looks are one thing to look at.
func (n *Node) signal() {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed {
		return
	}
	select {
	case n.events <- struct{}{}:
	default:
	}
}

func (s Status) clone() Status {
	out := s
	out.IPs = append([]string(nil), s.IPs...)
	out.CertDomains = append([]string(nil), s.CertDomains...)
	return out
}

// Forget deletes the node's identity from disk, so the next enable
// enrolls a fresh device. This is how the owner moves the backend to a
// different tailnet or a different account: the node key, the machine
// key and the logging id all live in that one directory and there is no
// other way to change which tailnet they belong to.
//
// It is the caller's job to have stopped the node first. This function
// cannot check — it takes a config root, not a Node — and deleting the
// state under a live node leaves a process holding an identity nothing
// on disk records.
func Forget(configRoot string) error {
	if strings.TrimSpace(configRoot) == "" {
		return fmt.Errorf("tailnet: no configuration directory, so there is no node state to remove")
	}
	if err := os.RemoveAll(StateDir(configRoot)); err != nil {
		return fmt.Errorf("tailnet: remove node state: %w", err)
	}
	return nil
}

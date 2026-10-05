package tailnet

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	Logf func(format string, args ...any)
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

// StateRunning is the backend state of a node that is fully up; it is
// tailscale's ipn.Running spelled without importing tailscale.
const StateRunning = "Running"

// Running reports whether the node is fully up.
func (s Status) Running() bool { return s.State == StateRunning }

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

// Candidate is an online tailnet peer, not an authenticated AO installation.
// Callers probe only its HTTPS endpoint and still require ordinary pairing.
type Candidate struct {
	Name    string
	DNSName string
	Address string
}

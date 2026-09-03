package settings

import (
	"fmt"
	"log"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
)

// NetworkSettings groups everything about how the embedded transport
// server is reached: which addresses it listens on, and what name and
// certificate it answers to (docs/specs/remote-access.md §7). Persisted
// as one nested object, so the whole group is one settings key with one
// write path.
type NetworkSettings struct {
	// BindAll, when true, asks the transport server to listen on
	// 0.0.0.0 so other devices on the LAN can reach the app. Default
	// false keeps the bind on 127.0.0.1 — the safe loopback behaviour.
	BindAll bool `json:"bindAll"`

	// ListenPort is the TCP port the transport binds. Zero means
	// automatic, which is what every install did before this existed:
	// the backend takes an ephemeral port on its first launch and then
	// keeps re-taking the same one out of the transport-port cache.
	//
	// A non-zero value is the operator saying which port this install
	// OWNS, and it is the only way to make that stable across a machine
	// where something else might get there first. It is worth setting on
	// a serve host and nowhere else: every share URL, every pairing link
	// and every paired client's stored endpoint names this number, so a
	// host reachable at a port somebody else picked is a host whose
	// address is only knowable by reading it off the console.
	//
	// The whole 1-65535 range is allowed, privileged ports included. A
	// backend with CAP_NET_BIND_SERVICE (or a launchd socket) can hold
	// port 443, and refusing that here would be guessing about a
	// capability this package cannot see. A bind that is not permitted
	// fails loudly at boot, naming the port.
	ListenPort int `json:"listenPort,omitempty"`

	// CanonicalDomain is the one HTTPS name this backend answers to: a
	// bare hostname, no scheme, no port, no path. Setting it does three
	// things at once — the Host header carrying that name is accepted,
	// `https://<domain>` joins the WebSocket origin allow-list, and the
	// share URL becomes that name instead of an IP. Whether a
	// certificate for it exists is a separate question: the domain may
	// be terminated in front of the backend by somebody else's proxy,
	// which is a path the spec answers rather than refuses.
	CanonicalDomain string `json:"canonicalDomain,omitempty"`

	// ACMEDNSHook is the command that publishes and removes the DNS-01
	// challenge record, stored as argv (never a shell line). The backend
	// runs it as `<argv...> set <fqdn> <value>` and
	// `<argv...> clear <fqdn> <value>`; see internal/acmecert. A hook
	// plus a canonical domain is what turns issuance on. Empty means the
	// backend never orders a certificate.
	ACMEDNSHook []string `json:"acmeDnsHook,omitempty"`

	// ExternalCertFile and ExternalKeyFile are the escape hatch for a
	// certificate this backend did not obtain: a private CA, a corporate
	// PKI, or a copy renewed by a tool the user already runs. Absolute
	// paths to two PEM files. The pair WINS over ACME — when both are
	// set the backend serves them and never orders anything — so the
	// user who already has a certificate is not made to prove it to a
	// certificate authority.
	ExternalCertFile string `json:"externalCertFile,omitempty"`
	ExternalKeyFile  string `json:"externalKeyFile,omitempty"`

	// TailnetEnabled asks this backend to join the owner's tailnet as
	// its own node (docs/specs/remote-access.md §7, "Anywhere access").
	// Off by default and lazily initialized: while it is false nothing
	// is constructed, no state directory exists, and no goroutine runs.
	// Turning it on is a network exposure change, which is why the RPC
	// that writes this group demands a step-up proof.
	TailnetEnabled bool `json:"tailnetEnabled,omitempty"`

	// PreviewPorts is the owner's hand-named half of this machine's
	// preview set (docs/specs/remote-access.md §7, the port gateway):
	// ports the dev-server scan did not attribute to a thread but that
	// the owner nonetheless wants reachable from their other devices.
	// The attributed half is discovered per tick and never persisted;
	// only a deliberate choice lives here.
	//
	// Sorted and deduplicated on write, so two writes of the same set
	// produce the same file and the reconciler sees no change. Changing
	// it takes `access:admin` and no step-up: it exposes the owner's own
	// dev server to the owner's own devices, which is a smaller act than
	// changing what the transport itself binds.
	PreviewPorts []int `json:"previewPorts,omitempty"`

	// TailnetControlURL is the coordination server the node registers
	// with. Empty means the Tailscale service, which is what nearly
	// every install wants; a self-hosted control plane (Headscale) is
	// the reason this is configurable at all. An absolute http(s) URL.
	TailnetControlURL string `json:"tailnetControlUrl,omitempty"`
}

// WantsTailnet reports whether the tailnet node should be running. One
// predicate so the reconciler, the status line and the forget refusal
// cannot disagree about what "enabled" means.
func (n NetworkSettings) WantsTailnet() bool { return n.TailnetEnabled }

// WantsACME reports whether the backend should try to obtain and renew a
// certificate for the canonical domain itself. One predicate, so the
// renewal loop, the manual button and the status line cannot disagree
// about whether issuance is configured.
func (n NetworkSettings) WantsACME() bool {
	return n.CanonicalDomain != "" && len(n.ACMEDNSHook) > 0 && !n.HasExternalPair()
}

// HasExternalPair reports whether both halves of the external
// certificate are configured. One half is refused by validation, so this
// is a whole-or-nothing answer everywhere it is read.
func (n NetworkSettings) HasExternalPair() bool {
	return n.ExternalCertFile != "" && n.ExternalKeyFile != ""
}

// MaxListenPort is the top of the TCP port range. Named rather than
// spelled, because the refusal message quotes it and a reader should see
// the same number in both places.
const MaxListenPort = 65535

// MaxACMEDNSHookArgs bounds the stored argv. A DNS hook is a command
// plus a handful of flags; a list past this length is a paste accident,
// and the bound keeps one settings read from carrying an unbounded
// argument vector to every attached client.
const MaxACMEDNSHookArgs = 32

// MaxPreviewPorts bounds the hand-named preview set. Every port in it
// holds a listener and a row in the dev-server list, so the bound is
// what keeps one settings write from asking the machine for an unbounded
// number of binds.
const MaxPreviewPorts = 64

// validateNetwork is the strict path, used by SetNetwork and by the
// whole-struct validation. Every rule here answers a question the
// serving side would otherwise have to answer at handshake time, where
// there is nobody to tell.
func validateNetwork(n NetworkSettings) (NetworkSettings, error) {
	if n.ListenPort < 0 || n.ListenPort > MaxListenPort {
		// Refused rather than clamped. A clamp would bind SOME port and
		// report success, and the operator would then be looking for
		// their backend at a number nothing ever chose.
		return NetworkSettings{}, fmt.Errorf(
			"network.listenPort must be 0 (automatic) or 1-%d, got %d", MaxListenPort, n.ListenPort)
	}

	if len(n.PreviewPorts) > MaxPreviewPorts {
		return NetworkSettings{}, fmt.Errorf(
			"network.previewPorts has %d entries, max is %d", len(n.PreviewPorts), MaxPreviewPorts)
	}
	previewPorts := make([]int, 0, len(n.PreviewPorts))
	seenPort := make(map[int]struct{}, len(n.PreviewPorts))
	for _, port := range n.PreviewPorts {
		if port < 1 || port > MaxListenPort {
			// Refused rather than dropped, on the strict path: the caller
			// is a person naming a port, and silently losing the one they
			// typed would leave them looking for a link that never
			// appears.
			return NetworkSettings{}, fmt.Errorf(
				"network.previewPorts entries must be 1-%d, got %d", MaxListenPort, port)
		}
		if _, dupe := seenPort[port]; dupe {
			continue
		}
		seenPort[port] = struct{}{}
		previewPorts = append(previewPorts, port)
	}
	sort.Ints(previewPorts)
	if len(previewPorts) == 0 {
		previewPorts = nil
	}
	n.PreviewPorts = previewPorts

	n.CanonicalDomain = strings.ToLower(strings.TrimSpace(n.CanonicalDomain))
	if n.CanonicalDomain != "" {
		if err := validateBareHostname(n.CanonicalDomain); err != nil {
			return NetworkSettings{}, fmt.Errorf("network.canonicalDomain: %w", err)
		}
	}

	if len(n.ACMEDNSHook) > MaxACMEDNSHookArgs {
		return NetworkSettings{}, fmt.Errorf(
			"network.acmeDnsHook has %d arguments, max is %d",
			len(n.ACMEDNSHook), MaxACMEDNSHookArgs,
		)
	}
	hook := make([]string, 0, len(n.ACMEDNSHook))
	for _, raw := range n.ACMEDNSHook {
		arg := strings.TrimSpace(raw)
		if arg == "" {
			// A blank argument would be passed to the process as an empty
			// string, which no DNS tool asked for and which shifts every
			// argument after it.
			return NetworkSettings{}, fmt.Errorf("network.acmeDnsHook contains a blank argument")
		}
		hook = append(hook, arg)
	}
	if len(hook) == 0 {
		hook = nil
	}
	n.ACMEDNSHook = hook
	if len(n.ACMEDNSHook) > 0 && n.CanonicalDomain == "" {
		return NetworkSettings{}, fmt.Errorf(
			"network.acmeDnsHook needs network.canonicalDomain: there is no name to order a certificate for",
		)
	}

	n.ExternalCertFile = strings.TrimSpace(n.ExternalCertFile)
	n.ExternalKeyFile = strings.TrimSpace(n.ExternalKeyFile)
	switch {
	case n.ExternalCertFile == "" && n.ExternalKeyFile == "":
	case n.ExternalCertFile == "":
		return NetworkSettings{}, fmt.Errorf("network.externalCertFile is required alongside network.externalKeyFile")
	case n.ExternalKeyFile == "":
		return NetworkSettings{}, fmt.Errorf("network.externalKeyFile is required alongside network.externalCertFile")
	default:
		for _, pair := range []struct{ key, path string }{
			{"network.externalCertFile", n.ExternalCertFile},
			{"network.externalKeyFile", n.ExternalKeyFile},
		} {
			if !filepath.IsAbs(pair.path) {
				// The backend's working directory is whatever launched it,
				// and it is not the same on a relaunch.
				return NetworkSettings{}, fmt.Errorf("%s must be an absolute path, got %q", pair.key, pair.path)
			}
		}
		if n.CanonicalDomain == "" {
			// The certificate is chosen by SNI against the canonical
			// domain, so a pair with no domain is a file nothing can
			// select.
			return NetworkSettings{}, fmt.Errorf(
				"network.externalCertFile needs network.canonicalDomain: the certificate is served for that name",
			)
		}
	}

	n.TailnetControlURL = strings.TrimSpace(n.TailnetControlURL)
	if n.TailnetControlURL != "" {
		// Validated here rather than at bring-up, because a control URL
		// the node cannot use leaves it parked waiting for a sign-in
		// that never arrives — minutes after the write that caused it,
		// with nothing connecting the two.
		parsed, err := url.Parse(n.TailnetControlURL)
		if err != nil {
			return NetworkSettings{}, fmt.Errorf("network.tailnetControlUrl: %w", err)
		}
		if parsed.Scheme != "http" && parsed.Scheme != "https" {
			return NetworkSettings{}, fmt.Errorf(
				"network.tailnetControlUrl scheme must be http:// or https://, got %q", parsed.Scheme)
		}
		if parsed.Host == "" {
			return NetworkSettings{}, fmt.Errorf("network.tailnetControlUrl is missing a host")
		}
	}

	return n, nil
}

// sanitizeNetwork is the lenient load-time counterpart. A hand-edited
// file with one unusable value loses that value with a log line rather
// than stranding the rest of the group — the same posture as the GitLab
// host allowlist. Dropping is safe in both directions here: without a
// domain the backend serves its self-signed certificate, which is what
// it did before any of this was configured.
//
// Each independent half is re-checked on its own, because they share
// nothing: a domain typo must not quietly take the node off the tailnet,
// and a control-URL typo must not quietly drop a working certificate
// setup. A half drops WHOLE when its value is unusable — for the
// tailnet, enabled bit included, since an empty control URL means the
// public coordination server and keeping the toggle alone would register
// this node somewhere the user did not name.
//
// The BIND half is two values now and only one of them can be wrong:
// BindAll is a bool and is always kept, while an out-of-range listen port
// drops to zero. Dropping it means "automatic", which is the behaviour
// every install had before the field existed, so the backend still binds
// and still starts — and the log line above says which value went.
//
// The PREVIEW half drops whole rather than per entry. A hand-edited list
// with one impossible number in it is a file somebody was editing, and
// keeping the rest would leave them reading a half-applied set with
// nothing on screen saying so; the log line names the value, and the
// setting is re-typed once.
func sanitizeNetwork(n NetworkSettings) NetworkSettings {
	sanitized, err := validateNetwork(n)
	if err == nil {
		return sanitized
	}
	log.Printf("settings: dropping unusable network configuration: %v", err)
	kept := NetworkSettings{BindAll: n.BindAll}
	if bind, bindErr := validateNetwork(NetworkSettings{ListenPort: n.ListenPort}); bindErr == nil {
		kept.ListenPort = bind.ListenPort
	}
	if tls, tlsErr := validateNetwork(NetworkSettings{
		CanonicalDomain:  n.CanonicalDomain,
		ACMEDNSHook:      n.ACMEDNSHook,
		ExternalCertFile: n.ExternalCertFile,
		ExternalKeyFile:  n.ExternalKeyFile,
	}); tlsErr == nil {
		kept.CanonicalDomain = tls.CanonicalDomain
		kept.ACMEDNSHook = tls.ACMEDNSHook
		kept.ExternalCertFile = tls.ExternalCertFile
		kept.ExternalKeyFile = tls.ExternalKeyFile
	}
	if preview, previewErr := validateNetwork(NetworkSettings{PreviewPorts: n.PreviewPorts}); previewErr == nil {
		kept.PreviewPorts = preview.PreviewPorts
	}
	if tailnet, tailnetErr := validateNetwork(NetworkSettings{
		TailnetEnabled:    n.TailnetEnabled,
		TailnetControlURL: n.TailnetControlURL,
	}); tailnetErr == nil {
		kept.TailnetEnabled = tailnet.TailnetEnabled
		kept.TailnetControlURL = tailnet.TailnetControlURL
	}
	return kept
}

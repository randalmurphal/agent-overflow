package settings

import (
	"fmt"
	"log"
	"path/filepath"
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
}

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

// MaxACMEDNSHookArgs bounds the stored argv. A DNS hook is a command
// plus a handful of flags; a list past this length is a paste accident,
// and the bound keeps one settings read from carrying an unbounded
// argument vector to every attached client.
const MaxACMEDNSHookArgs = 32

// validateNetwork is the strict path, used by SetNetwork and by the
// whole-struct validation. Every rule here answers a question the
// serving side would otherwise have to answer at handshake time, where
// there is nobody to tell.
func validateNetwork(n NetworkSettings) (NetworkSettings, error) {
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

	return n, nil
}

// sanitizeNetwork is the lenient load-time counterpart. A hand-edited
// file with one unusable value loses that value with a log line rather
// than stranding the rest of the group — the same posture as the GitLab
// host allowlist. Dropping is safe in both directions here: without a
// domain the backend serves its self-signed certificate, which is what
// it did before any of this was configured.
func sanitizeNetwork(n NetworkSettings) NetworkSettings {
	sanitized, err := validateNetwork(n)
	if err == nil {
		return sanitized
	}
	log.Printf("settings: dropping unusable network TLS configuration: %v", err)
	return NetworkSettings{BindAll: n.BindAll}
}

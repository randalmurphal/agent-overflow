package deviceclient

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// LinkVersion is the payload version this build can redeem. A second
// version is a different number and a different parse, never a flag inside
// the same shape — so a payload naming one this build does not know is
// refused rather than partially read.
const LinkVersion = 1

// LinkFragmentPrefix is what the redeeming surface looks for in a URL
// fragment. A fragment, not a query: it is never sent to a server, never
// written to an access log, and never lands in a Referer header.
const LinkFragmentPrefix = "pair="

// Link is what the minting surface handed this device: the decoded pairing
// payload.
//
// The producer is `internal/identity.PairingPayload`, restated here for the
// reason the wire constants are (see the package doc). Additive-only on
// both sides, and `wire_drift_test.go` round-trips a payload the real
// encoder produced through this decoder so a field added there without a
// field here is a failing test rather than a value silently dropped.
type Link struct {
	// Version is LinkVersion.
	Version int `json:"v"`
	// BackendID names the backend that minted this link. It is the key
	// this device files the resulting session under, so one installation
	// can hold sessions on several backends at once.
	BackendID string `json:"backendId"`
	// BackendName is the display name shown while pairing. Convenience
	// only: it grants nothing and is matched against nothing.
	BackendName string `json:"backendName,omitempty"`
	// Endpoint is the base URL to redeem against, scheme and authority.
	Endpoint string `json:"endpoint"`
	// Token is the single-use link token.
	Token string `json:"token"`
	// CertFingerprint is the backend's TLS certificate fingerprint,
	// "sha256:<lowercase hex>" over the leaf DER. Present when the boot
	// resolved a certificate, and what makes this a PINNING client rather
	// than a trusting one.
	//
	// Empty is a normal link, not a broken one: it is what a boot with no
	// certificate mints, and it lands on the trust-on-first-use path the
	// spec describes — safe on proof-of-possession plus the verification
	// number, never on channel secrecy.
	CertFingerprint string `json:"certFingerprint,omitempty"`
}

// DecodeLink reads a pairing link in every form a person can hand a
// terminal, because all three are things the pairing surface actually
// produces or a person actually copies:
//
//   - the full URL the settings pane shows, whose fragment carries it;
//   - `#pair=<payload>`, which is what a person selects when they copy
//     "the part after the address";
//   - the bare base64url payload, which is what a QR reader or a typed
//     code yields.
//
// The three collapse into one rule: everything before the first `#` is
// address and is dropped, and a `pair=` prefix on what is left is dropped
// too. A bare payload contains neither character, so it passes through
// untouched.
func DecodeLink(raw string) (Link, error) {
	encoded := strings.TrimSpace(raw)
	if _, fragment, found := strings.Cut(encoded, "#"); found {
		encoded = fragment
	}
	encoded = strings.TrimPrefix(encoded, LinkFragmentPrefix)
	if encoded == "" {
		return Link{}, fmt.Errorf("deviceclient: this is not a pairing link")
	}

	decoded, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return Link{}, fmt.Errorf("deviceclient: this pairing link is damaged; ask for a new one")
	}
	var link Link
	if err := json.Unmarshal(decoded, &link); err != nil {
		return Link{}, fmt.Errorf("deviceclient: this pairing link is damaged; ask for a new one")
	}
	if link.Version != LinkVersion {
		return Link{}, fmt.Errorf(
			"deviceclient: this pairing link is version %d and this build reads version %d; update both sides",
			link.Version, LinkVersion)
	}
	if link.Token == "" {
		return Link{}, fmt.Errorf("deviceclient: this pairing link carries no token; ask for a new one")
	}
	if link.BackendID == "" {
		return Link{}, fmt.Errorf("deviceclient: this pairing link names no backend; ask for a new one")
	}
	if _, err := dialBase(link.Endpoint, link.CertFingerprint); err != nil {
		return Link{}, err
	}
	return link, nil
}

// dialBase resolves the base URL this client actually dials, which is not
// always the one the payload printed.
//
// A pairing payload's endpoint is `http://` because the SPA's share URL is
// (a browser cannot pin a self-signed certificate, so the cleartext half
// of the listener is what it gets). A Go process owns its TLS
// configuration, and the SAME port answers both halves — the first byte of
// a connection decides (`internal/transport/tlssniff.go`). So a payload
// that carried a fingerprint is a payload whose backend terminates TLS on
// that authority, and dialling the cleartext half anyway would throw away
// the encryption the fingerprint exists to anchor.
//
// The promotion is therefore driven by the fingerprint and by nothing
// else: with one, https and a pin; without one, exactly the scheme the
// payload named. There is no in-between state where this client is
// encrypted but unverified.
func dialBase(endpoint, certFingerprint string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil {
		return "", fmt.Errorf("deviceclient: %q is not an address this client can dial", endpoint)
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("deviceclient: %q names no host", endpoint)
	}
	switch parsed.Scheme {
	case "http":
		if certFingerprint != "" {
			parsed.Scheme = "https"
		}
	case "https":
	default:
		return "", fmt.Errorf("deviceclient: %q is not an http or https address", endpoint)
	}
	// Path, query and fragment are dropped: the endpoint is an authority
	// this client appends its own routes to, and anything else riding it
	// would end up spliced into /auth/pair.
	return parsed.Scheme + "://" + parsed.Host, nil
}

// webSocketURL turns a dial base into the upgrade endpoint, carrying the
// ticket that names the session when one is supplied.
//
// wss for https and ws for http, so the socket rides the same half of the
// listener every other request from this client does. A pinned client that
// dialled the cleartext socket would hold an encrypted manifest fetch and
// a plaintext event stream, which is the worse half of both.
//
// An empty ticket yields the bare endpoint, which is what a caller that
// mints its OWN ticket per upgrade wants — the `--connect` stub's reverse
// proxy is configured once and re-tickets every carried handshake, so
// baking one in would be a credential going stale in a field.
func webSocketURL(base, ticket string) (string, error) {
	parsed, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("deviceclient: parse the endpoint: %w", err)
	}
	switch parsed.Scheme {
	case "https":
		parsed.Scheme = "wss"
	case "http":
		parsed.Scheme = "ws"
	default:
		return "", fmt.Errorf("deviceclient: %q is not an http or https address", base)
	}
	parsed.Path = wsPath
	if ticket != "" {
		parsed.RawQuery = url.Values{WSTicketParam: []string{ticket}}.Encode()
	}
	return parsed.String(), nil
}

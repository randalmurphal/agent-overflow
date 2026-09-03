// Package acmecert issues and renews the certificate for this backend's
// canonical domain, over ACME's DNS-01 challenge
// (docs/specs/remote-access.md §7, path 1: "owned domain + DNS-01").
//
// The domain is the one path that gives a plain browser real HTTPS. A
// browser cannot pin, so internal/servercert's self-signed certificate
// buys it nothing; a name the user owns, pointed at this machine (public
// DNS may hold a private address), with a certificate a trust store
// already accepts, is what removes the warning and what a passkey RP id
// will need. This package produces exactly that certificate. Which one
// the listener PRESENTS for a given handshake is
// internal/transport.CertificateSource's answer, and installing it is
// internal/app's job.
//
// # Why DNS-01, and why a hook
//
// DNS-01 is the only challenge type that works for a name resolving to
// an address the CA cannot reach — a LAN address, a tailnet address, a
// machine behind NAT — which is the whole population this feature is
// for. HTTP-01 and TLS-ALPN-01 both require the CA to connect inbound.
//
// Proving control of a DNS record means writing one, and every DNS
// provider's API is different. Rather than vendor a provider library per
// registrar, this package runs a command the USER configured:
//
//	<argv...> set   _acme-challenge.<domain> <txt-value>
//	<argv...> clear _acme-challenge.<domain> <txt-value>
//
// Two invocations, four arguments appended to whatever argv the user
// stored. The value is passed on `clear` as well as `set` so a hook can
// delete one record rather than every TXT record at that name, which
// matters when two issuances overlap. The hook is argv, never a shell
// line: nothing here is parsed or expanded, and a hook that wants a
// shell asks for one (`sh -c '…'`), the same contract and for the same
// reason as internal/worktreesetup. It runs unattended, in its own
// process group, bounded by a timeout, with stdout and stderr captured
// into the error when it fails — a hook that failed silently would look
// like the CA refusing to validate.
//
// # The escape hatch is elsewhere
//
// A private CA (mkcert-style, manually trusted) or a certificate some
// other tool renews is configured as a cert/key file pair in settings,
// and internal/app loads it directly. It deliberately does not come
// through this package: nothing is issued, so there is no order, no
// account, and no renewal to run. An external pair also WINS over ACME —
// a user who supplied their own certificate has said which one they
// want.
//
// # What is testable here, and what is not
//
// The order flow is driven through the narrow CA interface below, so the
// tests cover every seam that is ours: the TXT value handed to the hook,
// the set → validate → clear ordering, clear-on-failure, the account key
// round-tripping (a key that changed would change every TXT value), the
// persistence format, and the renewal window. Nothing in this package's
// tests reaches a network.
//
// A real issuance against Let's Encrypt is inherently live-only: it
// needs a domain, DNS the CA resolves, and a rate-limited production
// endpoint. There is no staging fixture here and no fake CA that would
// tell you your hook works. That check is the "Issue now" button in
// Settings → Remote access, and its failure text names the stage that failed.
package acmecert

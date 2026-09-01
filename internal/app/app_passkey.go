package app

import (
	"net"
	"strings"

	"agent-overflow/internal/appidentity"
	"agent-overflow/internal/identity"
)

// The passkey seam (docs/specs/remote-access.md §4 "Step-up", "Passkeys").
//
// internal/identity runs the ceremonies and internal/transport carries the
// bytes; what neither of them can know is what this backend ANSWERS TO,
// because that is a setting the owner edits and a listener the boot bound.
// This file resolves it, and satisfies the transport's step-up hook over
// the same core.

// passkeyRelyingParty resolves the relying party every ceremony runs
// under, from the canonical domain in Settings → Network and the live
// listener's port.
//
// The canonical domain is the ONLY candidate for an RP ID. WebAuthn
// requires a domain — an address is refused outright, and an authenticator
// binds a credential to the exact string, so a name that changes with the
// network (a LAN IP, a tailnet address) would silently orphan every
// credential the moment the machine moved. A backend with no domain
// therefore has no passkey surface at all, which the surfaces say plainly
// rather than half-offering.
//
// The `.localhost` family is the exception and the harness's path: a
// browser treats those names as a secure context over plain HTTP and
// resolves them to loopback itself, so they are the only names a ceremony
// can run under with no certificate at all. `localhost` bare is not
// settable — a canonical domain must contain a dot — so the spelling that
// works is `<label>.localhost`.
func passkeyRelyingParty(a *App) identity.RelyingParty {
	if a == nil || a.settings == nil {
		return identity.RelyingParty{}
	}
	domain := strings.TrimSpace(a.settings.Get().Network.CanonicalDomain)
	if domain == "" {
		return identity.RelyingParty{}
	}
	port := ""
	if srv := a.transportServer.Load(); srv != nil {
		if _, p, err := net.SplitHostPort(srv.Addr()); err == nil {
			port = p
		}
	}
	return identity.RelyingParty{
		ID: domain,
		// What the authenticator's prompt calls this backend. The stable
		// product name, deliberately not appidentity.AppTitle — that varies
		// by boot mode, and an authenticator stores this string beside the
		// credential, so a harness boot would leave a person's passkey list
		// naming two things that are one backend.
		DisplayName: appidentity.Name,
		Origins:     passkeyOrigins(domain, port),
	}
}

// passkeyOrigins is every origin a page running a ceremony may be loaded
// from, for one domain.
//
// Origins are compared scheme-and-authority exact, and a PORT is part of
// an authority, so naming only one would refuse the ceremony this backend
// had just started whenever the browser reached it by the other route.
// Two exist:
//
//   - the direct bind, `https://<domain>:<port>`, which is how a browser
//     reaches a backend serving its own domain certificate;
//   - the default port, `https://<domain>`, which is what a browser sends
//     when a reverse proxy on this machine terminates TLS on 443 and
//     forwards to the loopback bind.
//
// A proxy on some OTHER port is not derivable from anything this process
// knows, and is the deployment that has to be reached on the direct
// authority instead.
//
// The `.localhost` family is the one that runs over cleartext, because a
// browser treats those names as a secure context and WebAuthn is
// unavailable outside one. Every other name is https, since a ceremony
// would not have started.
func passkeyOrigins(domain, port string) []string {
	scheme := "https"
	if isLocalhostName(domain) {
		scheme = "http"
	}
	origins := []string{scheme + "://" + domain}
	if port != "" {
		origins = append(origins, scheme+"://"+net.JoinHostPort(domain, port))
	}
	return origins
}

// isLocalhostName reports whether a browser treats this name as loopback
// and as a secure context without a certificate.
//
// Deliberately NOT loopback.HostHeader, which is a different rule with a
// different job: it decides which Host headers this listener answers, and
// it accepts address literals — which are exactly what an RP ID may never
// be. The two are not interchangeable, and the loopback package's own doc
// says so about every pair in it.
func isLocalhostName(domain string) bool {
	return domain == "localhost" || strings.HasSuffix(domain, ".localhost")
}

// StepUpProof spends the step-up token an RPC presented and reports
// whether it proved this session. Satisfies transport.Config.StepUpProof.
//
// The token is consumed by the asking, whatever the answer — that is what
// single use means, and it is why the transport asks once per call and
// carries the result rather than letting each gate re-ask.
func StepUpProof(a *App, sessionID, token string) bool {
	state := a.identityState()
	if state == nil {
		return false
	}
	return state.sessions.SpendStepUpToken(sessionID, token)
}

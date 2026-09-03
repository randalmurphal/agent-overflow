package transport

import (
	"crypto/tls"
	"errors"
	"strings"
	"sync/atomic"
)

// CertificateSource is the set of certificates this listener may present,
// resolved per handshake.
//
// Two certificates, and which one answers is decided by the name the
// client asked for:
//
//   - The SELF-SIGNED certificate (internal/servercert) answers every
//     handshake that did not name the canonical domain — no SNI at all, an
//     IP literal, `localhost`, any other name. Nothing trusts it; its
//     value is that the pairing payload carries its fingerprint and a
//     client owning its own TLS configuration pins those exact bytes
//     (docs/specs/remote-access.md §7). That is why it must stay the
//     default: a pinning client's SNI is whatever address it was paired
//     on, and handing it a domain certificate instead would read as the
//     backend having been replaced.
//   - The DOMAIN certificate (an ACME issuance or a private-CA pair the
//     user configured) answers a handshake whose ServerName IS the
//     configured canonical domain. That is the browser's path: a real
//     name, a certificate a trust store accepts, and no prompt.
//
// Both slots are swappable while the listener runs. A renewal, a first
// issuance, or a domain the user just changed takes effect on the NEXT
// handshake — no rebind, no restart. That is the whole reason resolution
// is a function rather than a fixed Certificates slice: a certificate
// this backend renews itself would otherwise need a listener swap to
// serve, on a schedule nobody is watching.
//
// The zero value is not usable; construct with NewCertificateSource so
// the atomics are the ones the server reads.
type CertificateSource struct {
	selfSigned atomic.Pointer[tls.Certificate]
	domain     atomic.Pointer[domainCertificate]
}

// domainCertificate pairs the certificate with the name it answers for.
// One pointer holding both, so a SetDomain racing a handshake can never
// present the new certificate under the old name (or the reverse): the
// swap is one store.
type domainCertificate struct {
	// name is the canonical domain, lower-cased once at set time. SNI
	// comparison is case-insensitive and happens per handshake, so the
	// fold belongs here rather than in the hot path.
	name string
	cert *tls.Certificate
}

// errNoCertificate refuses a handshake that arrived before any
// certificate did. It is deliberately a refusal and not a panic or a nil
// return: the sniff wrapper installs whenever a source exists, so this
// is the ordinary state of a boot whose certificate is still resolving,
// and the peer gets a failed handshake it will retry rather than a dead
// process.
var errNoCertificate = errors.New("transport: no TLS certificate is loaded yet")

// NewCertificateSource returns an empty source. Certificates are
// installed afterwards, in any order and at any time.
func NewCertificateSource() *CertificateSource {
	return &CertificateSource{}
}

// SetSelfSigned installs (or, with nil, clears) the backend's own
// self-signed certificate — the one whose fingerprint pairing payloads
// carry.
func (s *CertificateSource) SetSelfSigned(cert *tls.Certificate) {
	s.selfSigned.Store(cert)
}

// SetDomain installs the certificate that answers for the canonical
// domain. An empty name or a nil certificate clears the slot, which is
// what a user who removed their domain, or an issuance that has not
// happened yet, leaves behind — the self-signed certificate then answers
// every handshake again, exactly as it did before a domain existed.
func (s *CertificateSource) SetDomain(name string, cert *tls.Certificate) {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" || cert == nil {
		s.domain.Store(nil)
		return
	}
	s.domain.Store(&domainCertificate{name: name, cert: cert})
}

// ServesDomain reports whether a certificate for THIS name is loaded
// right now. The share URL asks, because publishing an https:// URL for
// a name nothing can complete a handshake on is worse than publishing
// the http:// one.
//
// The name is part of the question and not a detail: a user who changes
// their domain has a settings record naming the new one while this
// listener still holds the old certificate, and a bare "is a domain
// certificate loaded" would answer yes and publish a URL that cannot
// connect for as long as issuance takes.
func (s *CertificateSource) ServesDomain(name string) bool {
	domain := s.domain.Load()
	if domain == nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(name), domain.name)
}

// certificateFor answers one ClientHello. It is the tls.Config's
// GetCertificate, so it runs on the handshake goroutine and does nothing
// but two atomic loads and a string compare.
func (s *CertificateSource) certificateFor(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	if hello != nil && hello.ServerName != "" {
		if domain := s.domain.Load(); domain != nil {
			if strings.EqualFold(hello.ServerName, domain.name) {
				return domain.cert, nil
			}
		}
	}
	if cert := s.selfSigned.Load(); cert != nil {
		return cert, nil
	}
	// A domain certificate exists but the handshake named something else,
	// and there is no self-signed certificate to fall back on. Presenting
	// the domain certificate anyway would hand a pinning client bytes it
	// never pinned, which it reads as a different backend.
	return nil, errNoCertificate
}

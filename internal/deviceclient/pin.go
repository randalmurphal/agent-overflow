package deviceclient

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"time"

	"agent-overflow/internal/servercert"
)

// ErrCertificateMismatch means the backend at this address presented a
// certificate other than the one this device pinned when it paired.
//
// It travels wrapped inside whatever crypto/tls and net/http put around a
// handshake failure, so callers test for it with errors.Is rather than by
// reading a message. What it is NOT is a reason to fall back: a client
// that dialled on anyway would have exactly the trust a client with no pin
// has, which is the property the pairing ceremony bought.
var ErrCertificateMismatch = errors.New(
	"deviceclient: this backend presented a different TLS certificate than the one this device pinned; " +
		"if the backend was reinstalled or its certificate replaced, pair this device again")

// pinTimeout bounds one credential exchange end to end. Every call this
// client makes is a small JSON round trip on a LAN, and the one that runs
// inline on a carried WebSocket upgrade is the reason there is a bound at
// all: a stalled mint must fail fast enough for the page's reconnect
// ladder to own the retry.
const pinTimeout = 10 * time.Second

// pinnedTLSConfig is what a Go-native client does with the fingerprint the
// pairing payload handed it.
//
// InsecureSkipVerify with a VerifyPeerCertificate is stricter than chain
// verification, not weaker than it: the certificate is self-signed, so
// there is no chain to build and no CA whose judgement could be
// substituted for the owner's. What replaces it is an equality check
// against bytes the owner's own machine published during a ceremony that
// already established trust.
//
// A nil answer is the OTHER supported posture and not an absence of one:
// no fingerprint means ordinary WebPKI verification, which is what the
// owned-domain path (§7, DNS-01) produces. There is deliberately no third
// state in which this client is encrypted and verifying nothing.
func pinnedTLSConfig(certFingerprint string) *tls.Config {
	if certFingerprint == "" {
		return nil
	}
	return &tls.Config{
		InsecureSkipVerify: true, //nolint:gosec // VerifyPeerCertificate below is the stricter check.
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return fmt.Errorf("%w: it presented no certificate at all", ErrCertificateMismatch)
			}
			// The LEAF only. A self-signed certificate is its own chain,
			// and a peer that sent extra certificates cannot make one of
			// them the identity by putting it later in the list.
			if presented := servercert.Fingerprint(rawCerts[0]); presented != certFingerprint {
				return fmt.Errorf("%w (presented %s, pinned %s)",
					ErrCertificateMismatch, presented, certFingerprint)
			}
			return nil
		},
		// The floor the backend's own listener sets. Both ends of this
		// connection are current Go processes, so nothing older is being
		// excluded that could have connected anyway.
		MinVersion: tls.VersionTLS12,
	}
}

// pinnedTransport is the RoundTripper every request this client makes goes
// through, including the WebSocket upgrade a `--connect` stub carries.
//
// One transport per client rather than one per call: connection reuse is
// what keeps a poll loop from repeating a TLS handshake every few seconds,
// and the pin is a property of the transport, so sharing it is also what
// makes "every request from this device is verified" true by construction
// rather than by every call site remembering.
func pinnedTransport(certFingerprint string) *http.Transport {
	cloned := &http.Transport{Proxy: http.ProxyFromEnvironment}
	if base, ok := http.DefaultTransport.(*http.Transport); ok {
		cloned = base.Clone()
	}
	cloned.TLSClientConfig = pinnedTLSConfig(certFingerprint)
	// ForceAttemptHTTP2 would ask for h2 by ALPN, and the backend answers
	// http/1.1 only because the `/ws` upgrade needs the raw-connection
	// takeover h2 does not offer. Asking for something the server will
	// never agree to costs a byte on every handshake and invites a
	// negotiated protocol this client cannot upgrade over.
	cloned.ForceAttemptHTTP2 = false
	return cloned
}

// Package deviceclient is the Go-native paired device: the client half of
// the device surface `internal/identity` serves
// (docs/specs/remote-access.md §4 and §7).
//
// A browser gets this from `frontend/src/lib/transport/deviceSession.ts`
// and `deviceKey.ts`. A Go process — `agent-overflow --connect`, and the
// desktop attach client behind it — cannot: it holds no localStorage, no
// IndexedDB and no `crypto.subtle`, and unlike a browser it OWNS its TLS
// configuration, which is the whole reason the pairing payload carries a
// certificate fingerprint at all. So this package is that half, built on
// files and `crypto/ecdsa`, following the browser module's rules because
// they are the backend's rules rather than the browser's.
//
// What it owns:
//
//   - The device key. ECDSA P-256, minted at enrollment ONLY, persisted
//     PKCS#8 PEM at 0600 under a caller-supplied profile directory.
//   - The proof. A compact JWS per request, matching what
//     `internal/identity/deviceproof.go` verifies.
//   - The pairing link. Decoding what the minting surface produced, in
//     every form a person can paste.
//   - The rotating session. One atomicfile JSON per backend id, renewed
//     single-flight, stored before use, never retried unread.
//   - The pin. A `tls.Config` that compares the leaf's fingerprint against
//     the one the pairing payload carried, and refuses on a mismatch with
//     re-pairing named as the remedy.
//
// # Layering
//
// This package speaks the WIRE, and imports neither `internal/identity`
// nor `internal/transport`. Identity persists into `internal/store`, and
// transport is store-free by construction; a client that imported either
// would drag a database or a server into every process that only wants to
// attach. The route names, the two header names and the ticket parameter
// are therefore RESTATED here, and `wire_drift_test.go` pins each spelling
// to the transport's own constant — the same arrangement, for the same
// reason, as `internal/relaysession`.
//
// The one first-party import is `internal/servercert`, for `Fingerprint`.
// That package is stdlib plus `internal/atomicfile`, and the value a
// device pins has to be the same spelling of the same digest the backend
// published; two implementations of "sha256 over the leaf DER" agree only
// until one of them is edited.
//
// # What a paired device may present
//
// Everything here enrolls `proof_kind = key`: the redemption carries a
// signed proof, so the thumbprint the backend records is derived from a
// key this process has just demonstrated it holds. There is deliberately
// no bearer path — that class exists for a plain-HTTP LAN page with no
// secure context (spec §15 constraint 6), and a Go process is never one.
package deviceclient

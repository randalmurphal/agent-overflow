package store

// deviceProofKindV77SQL records HOW a device proves it holds the key its
// row names (docs/specs/remote-access.md §4, phase 5).
//
// Until this column existed, `key_thumbprint` carried two different things
// under one name. A device with a real WebCrypto keypair and a device that
// minted 32 random bytes because its page was not a secure context both
// stored a string here, and `CheckDeviceProof` compared that string. So a
// device that had enrolled a real key could still be presented as a bearer
// value — the copy of a credential string was as good as the key.
//
// The column is what makes the two representable apart, and therefore what
// makes the stronger one enforceable:
//
//   - `bearer` — the row's thumbprint is an opaque enrollment identifier,
//     compared as a string. This is the plain-HTTP LAN browser class of
//     spec §15 constraint 6: no secure context, so no non-extractable
//     WebCrypto, so deliberately no signed-proof path. It is also what
//     every device paired before this migration holds, which is why it is
//     the DEFAULT: an existing device keeps working exactly as it did.
//   - `key` — the row's thumbprint is the RFC 7638 thumbprint of an ECDSA
//     P-256 public key, and the only accepted presentation is a signed
//     proof over the request (internal/identity/deviceproof.go).
//
// One ALTER TABLE with a default, not a rebuild: `devices` is an
// authoritative table and every existing row's answer is the default.
//
// The CHECK constrains rows written from here on. SQLite does not
// retro-validate on ADD COLUMN, which costs nothing — every pre-existing
// row holds the default, which is one of the two permitted values.
const deviceProofKindV77SQL = `
ALTER TABLE devices ADD COLUMN proof_kind TEXT NOT NULL DEFAULT 'bearer'
    CHECK(proof_kind IN ('bearer','key'));`

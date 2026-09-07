# Nearby pairing bootstrap

This package transports an ordinary single-use invitation; identity/store still
own pending enrollment, owner confirmation, activation and revocation. The host
must display this exchange's independently derived comparison value for its
mapped invitation, never the ordinary server-returned comparison value. Issuing
the encrypted invitation before confirmation is safe only because redemption
still creates a pending session that admits nothing until owner approval.

The initiator commits its fresh P-256 ephemeral key, random nonce and metadata
before seeing the responder contribution. The responder fixes its fresh reply
before the initiator reveals; each client object accepts only that one reply.
Both derive the comparison and encryption key from the complete ordered
transcript and ECDH result. Do not replace this with server-returned digits,
uncommitted short certificate hashes, or an uncommitted TLS-exporter comparison:
those allow relay substitution or offline grinding of matching short values.
The commitment/SAS precedent is RFC 6189 section 4.4.1.1; this is a small
bootstrap using that pattern, not an implementation of the ZRTP media protocol.

Discovery and bootstrap TLS are untrusted. Only this credential-free HTTP
client may skip certificate verification; the decrypted invitation's pin is
enforced by the existing device client before any token is sent. Never attach
session headers, an authenticated transport, proxy, or cookie jar here. The
wire never carries the plaintext invitation or comparison value. Owner-only
Snapshot must not be returned from a public discovery/info endpoint.

One owner-opened window and one requester bound memory and online guesses.
Retries replay the same challenge/ciphertext; another commitment requires an
explicit owner reopen. Close/expiry cancels an associated invitation, including
a mint finishing after cancellation. The caller's cancel callback must leave
already-confirmed pairings intact. Mint/cancel callbacks run outside the lock.
The App retains its last-link denial fence after close/expiry and refuses a
replacement window until cancellation is durable; a failed database write
must not restore ordinary confirmation for a retired SAS exchange.

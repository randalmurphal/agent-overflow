# internal/pairbootstrap

Credential-free bootstrap for an ordinary single-use pairing invitation.
Identity and storage own enrollment, confirmation, activation, and revocation.
See [computer-pairing.md](../../docs/architecture/computer-pairing.md).

- Display the comparison value derived from this exchange and its mapped
  invitation. Do not substitute the server pairing flow's comparison value.
- Preserve the committed P-256 ephemeral-key exchange and its ordered transcript.
  Each client instance accepts one responder contribution. The wire carries
  neither the plaintext invitation nor the comparison value.
- Bootstrap discovery and TLS are unauthenticated. This package is the only
  pairing path that may skip certificate verification. The decrypted
  invitation's certificate pin must be checked by `internal/deviceclient`
  before any credential is sent.
- Do not attach cookies, session headers, an authenticated transport, or an
  environment proxy to bootstrap requests.
- Keep one owner-opened exchange and one requester in memory. Retries reuse its
  challenge and ciphertext. Opening another exchange requires a new owner
  action.
- Close and expiry cancel any associated unconfirmed invitation, including a
  mint that completes late. Run mint and cancellation callbacks outside the
  package mutex. Already-confirmed pairings remain valid.

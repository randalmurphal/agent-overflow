# internal/transferwire/

The versioned, bounded computer-to-computer transfer contract. Stdlib only.
No history schemas, provider adapters, HTTP handlers or ownership policy here.

- Every reply names its backend and operation. Callers verify both.
- Grants and activation secrets are separate 256-bit values. The source alone
  holds activation until its retirement commits; a destination grant cannot
  activate history. Secrets never enter a URL or ordinary status reply.
- Version 1 changes add optional fields only. A new required meaning needs an
  explicit version/refusal before source retirement, not optimistic decoding.
- `needsAttention` reports a durable destination job failure without echoing
  arbitrary peer error text through the transfer client. The frontend obtains
  the detailed error from that computer's separately authorized status RPC.
- The transport route and byte-upload limit have drift tests against this
  contract. Control calls stay bound RPCs; archive bytes use transport's HTTP
  route so a large transfer cannot block conversation event sockets.

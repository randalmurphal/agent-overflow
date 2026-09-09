# internal/transferclient/

A client for one explicitly approved computer-to-computer handoff. It shares
deviceclient's TLS comparison against the exact saved certificate bytes, but
owns no device key or session.

- Offers bind endpoint, certificate, backend, operation and one-operation grant.
  Only HTTPS or literal loopback transport is admitted; loopback dialing cannot
  be redirected by DNS. Redirects never receive the grant or activation secret.
- Every acknowledgment must match both identities and the wire version, including
  errors. Replies are bounded; peer error prose is treated as data and is not displayed.
- A request is attempted once. The coordinator resolves unknown outcomes through
  durable status/checkpoints before retrying. Neither a timeout nor a missing
  response says whether the peer committed a mutation.
- Calls have deadlines and reuse a bounded connection pool. Close each operation's
  client. Offers belong only in private recovery data, never status or logs.
- A chunk owns a bounded byte slice until its call returns. Its request reader
  closes synchronously before returning even though net/http can close bodies
  asynchronously; otherwise the next chunk can overwrite a buffer a late writer
  still reads. `TestTransferChunkReturnFencesLateHTTPBodyReaders` pins this.

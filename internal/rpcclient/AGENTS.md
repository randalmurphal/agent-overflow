# internal/rpcclient/

Serialized HTTP+WebSocket RPC client shared by local owner commands and paired
computer calls. One outstanding call, 1 MiB frame bound, no event subscriptions
or retained history. The caller owns dialing, credentials and TLS. It must close
the client after a context/read/write failure; never retry a mutation here.

For a paired peer, read Hello and verify identity, protocol and capability on
the authenticated connection before sending an RPC. Hello is a narrow reader
projection of the wire; unknown additive fields are ignored. Local owner calls
may let Call skip the initial hello as before.

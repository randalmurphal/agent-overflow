# Own-device membership

This package is the bounded public membership shape and merge rule. It contains
no credential, private key, signing authority, network client or replica of
conversation state. One installation belongs to one personal device set; an
explicit personal pairing joins the sets on both sides.

A key thumbprint identifies a device; a serving backend UUID is optional (phones
have none). Membership generations change only on explicit restoration after
removal. Removal wins a concurrent update of the same generation. Old catalogs
cannot clear it. Metadata updates never restore membership. Forwarded metadata
may fill an unknown identity but cannot replace established endpoint trust;
only an authenticated direct source updates its own metadata.

The catalog is capped at 128 records, including removal tombstones. Never prune
tombstones merely because a device has been offline: an old catalog would restore
its access. A new explicit personal pairing can restore the same key in a newer
generation; old session rows stay revoked.

`internal/store` persists this security authority. `internal/identity` applies
membership and session revocation atomically, invalidates its admission cache,
then closes sockets after releasing its membership lock. Network cleanup can
call app cleanup hooks; never hold that lock across connection closure.

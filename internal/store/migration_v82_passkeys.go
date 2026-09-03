package store

// passkeysV82SQL adds the owner's passkey credentials and the WebAuthn
// user handle they are registered against (docs/specs/remote-access.md §4,
// "Passkeys").
//
// Authoritative rows, like every other member of the identity family: a
// dropped passkey row is a sign-in method somebody no longer has, and the
// recovery is re-pairing from a host-local surface. Nothing here may be
// pruned by a cache sweep.
//
// Two statements, one migration, because neither is meaningful alone. A
// credential row names a user handle, and a handle with no credential is
// 32 bytes nothing was ever registered under.
//
// # Why the handle is a column on `users`
//
// WebAuthn identifies an account by an opaque handle the authenticator
// stores alongside the credential, and a discoverable ("resident") login
// returns it instead of asking who is signing in. It is therefore a
// property of the ACCOUNT, exactly like `display_name`, and putting it on
// a table of its own would be a second place a user could exist. NULL
// means "no passkey ceremony has run for this account yet", which is
// every row before this migration and every account that never registers
// one — it is minted lazily by the first ceremony, not backfilled here,
// because minting it in SQL would hand every existing account a handle
// nothing can ever present.
//
// # Why credentials hang off the user, not off a device
//
// `devices.passkey_credential_id` has existed since v79 and stays unused
// by this wave. A passkey is not a device: one authenticator syncs across
// a person's phones, and a hardware key moves between machines in a
// pocket. Binding a credential to a device row would mean a synced passkey
// either names a device that is not the one signing in, or accumulates one
// device row per surface it appears on. Owner-level is what makes "sign in
// on a browser this backend has never seen" expressible at all — the
// DEVICE row that sign-in mints is a separate fact, resolved the way
// pairing resolves it.
//
// # The columns that are not obvious
//
//   - `credential_id` is the authenticator's own opaque id, stored raw and
//     uniquely indexed. It is what an assertion arrives naming, so it is
//     the lookup key; BLOB rather than an encoding because the library
//     hands us bytes and every re-encoding is a way for two spellings of
//     one credential to exist.
//   - `rp_id` records the domain the credential was registered under. A
//     passkey is bound to its RP ID by the authenticator and cannot be
//     presented to another one, so a backend whose canonical domain
//     changed holds rows that will never assert again. They are still
//     LISTED, with their RP ID, because a list that silently omitted them
//     would leave a person deleting credentials they cannot see.
//   - `sign_count` and `clone_warning` are the authenticator's counter and
//     the verdict on it. A counter that fails to advance is FLAGGED and
//     never refused (the {0,0} case is every platform authenticator that
//     does not keep one at all), so the bit is persisted and surfaced as
//     an anomaly rather than acted on. Refusing would sign a person out of
//     a working key on evidence that is routinely absent.
//   - `backup_eligible` / `backup_state` are the synced-passkey flags, and
//     they LATCH: eligibility is a fact about the credential decided at
//     registration, so a later assertion claiming a different one is a
//     different credential. Persisting them is what lets that comparison
//     happen at all.
//   - `user_verified` records whether registration verified the person
//     (a PIN, a fingerprint). Step-up demands verification on the
//     ASSERTION, read from that assertion's own flags; this column says
//     what the credential was enrolled with, which is what a list can
//     honestly show.
//   - `transports` is the JSON array the browser reported, passed back on
//     a later ceremony as a hint about how to reach the same
//     authenticator. Advisory: nothing authorizes on it.
//
// `last_used_at` is 0 for a credential that has never asserted, on the
// same rule as every other stamp in this family: Go reads NULL and 0 the
// same, so there is one spelling of "has not happened".
const passkeysV82SQL = `
ALTER TABLE users ADD COLUMN webauthn_user_handle BLOB NULL;

CREATE TABLE passkeys (
    id                 TEXT PRIMARY KEY,
    user_id            TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    label              TEXT NOT NULL,
    credential_id      BLOB NOT NULL,
    public_key         BLOB NOT NULL,
    attestation_type   TEXT NOT NULL DEFAULT '',
    attestation_format TEXT NOT NULL DEFAULT '',
    transports         TEXT NOT NULL DEFAULT '',
    aaguid             BLOB NULL,
    attachment         TEXT NOT NULL DEFAULT '',
    rp_id              TEXT NOT NULL,
    sign_count         INTEGER NOT NULL DEFAULT 0,
    clone_warning      INTEGER NOT NULL DEFAULT 0 CHECK(clone_warning IN (0,1)),
    user_verified      INTEGER NOT NULL DEFAULT 0 CHECK(user_verified IN (0,1)),
    backup_eligible    INTEGER NOT NULL DEFAULT 0 CHECK(backup_eligible IN (0,1)),
    backup_state       INTEGER NOT NULL DEFAULT 0 CHECK(backup_state IN (0,1)),
    created_at         INTEGER NOT NULL,
    last_used_at       INTEGER NOT NULL DEFAULT 0
);

CREATE UNIQUE INDEX idx_passkeys_credential_id ON passkeys(credential_id);
CREATE INDEX idx_passkeys_user ON passkeys(user_id, created_at);
`

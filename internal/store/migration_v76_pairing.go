package store

// pairingAndRefreshV76SQL adds what turns the v75 identity core into a
// credential lifecycle: how a new device is admitted (pairing links), how
// an admitted device stays admitted without a long-lived credential
// (rotating refresh secrets), and the two columns those two flows need on
// the tables v75 already created (docs/specs/remote-access.md §4).
//
// Authoritative, not cache — the same rule v75 states. A dropped pairing
// link is a device that cannot finish joining; a dropped refresh row is a
// device signed out mid-week. Neither may be pruned by a cache sweep, and
// the two prunes this package does offer (ExpiredPairingLinks,
// SpentRefreshSecrets) delete only rows that can never admit anything
// again.
//
// Four changes, one migration, because they are one flow: a pairing link
// mints the session, the session's first refresh secret is minted with it,
// and neither is presentable until the confirmation column says so.
//
//   - `devices.channel` names a device the BACKEND mints for itself rather
//     than one a person paired: today only the local page channel (the
//     embedded webview, the `--connect` stub, the WSL launcher relay),
//     which must resolve to the same device row on every boot instead of
//     accumulating one per launch. Empty for every paired device, and the
//     partial unique index is what makes "one row per channel" structural
//     rather than a convention the boot has to remember.
//   - `sessions.activated_at` is the confirmation gate. A pairing
//     redemption mints a real session and a real credential, and neither
//     admits anything until the owner has confirmed the verification
//     number on the minting surface (§4 "Pairing" step 4). NULL means "not
//     yet"; `Session.Live` reads it, so a session awaiting confirmation is
//     refused by the same predicate that refuses a revoked one, rather
//     than by a second check some later call site could forget. Existing
//     rows are backfilled from `created_at`: every session v75 minted was
//     live the moment it was written.
//   - `pairing_links` is the single-use admission ticket. The token is
//     stored as a hash for the same reason a recovery code is — the
//     backend never needs the value again, only the ability to recognise
//     it — and `redeemed_at` carries the single-use rule as a one-statement
//     compare-and-set predicate, so SQLite picks the winner of a race
//     between two redemptions rather than a caller-side read-then-write.
//   - `refresh_secrets` is the rotating half of the credential pair. One
//     row per issued secret; `consumed_at` is again the CAS predicate.
//     Spent rows are KEPT, and that is the whole reuse detector: a
//     presentation that matches a row whose `consumed_at` is set is a copy
//     of a secret the real device already spent, and the response is to
//     revoke the session it belongs to. Deleting spent rows would delete
//     the detector.
//
// `pairing_links.device_id` / `.session_id` and `refresh_secrets.consumed_by`
// are plain columns with no foreign key, matching `auth_audit`: a pairing
// row is the record of an exchange, and it is worth most after the device
// it admitted has been revoked and removed. `refresh_secrets.session_id`
// DOES cascade, because a refresh secret with no session is not history,
// it is a credential naming nothing.
const pairingAndRefreshV76SQL = `
ALTER TABLE devices ADD COLUMN channel TEXT NOT NULL DEFAULT '';
CREATE UNIQUE INDEX idx_devices_channel ON devices(channel) WHERE channel <> '';

ALTER TABLE sessions ADD COLUMN activated_at INTEGER NULL;
UPDATE sessions SET activated_at = created_at;

CREATE TABLE pairing_links (
    id               TEXT PRIMARY KEY,
    user_id          TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash       BLOB NOT NULL,
    scopes           TEXT NOT NULL CHECK(json_valid(scopes)),
    binding_class    TEXT NOT NULL CHECK(binding_class IN ('loopback-only','device-bound','public')),
    device_class     TEXT NOT NULL CHECK(device_class IN ('desktop','browser','phone','cli','backend-peer')),
    cert_fingerprint TEXT NOT NULL DEFAULT '',
    created_at       INTEGER NOT NULL,
    expires_at       INTEGER NOT NULL,
    redeemed_at      INTEGER NULL,
    device_id        TEXT NOT NULL DEFAULT '',
    key_thumbprint   TEXT NOT NULL DEFAULT '',
    session_id       TEXT NOT NULL DEFAULT '',
    confirmed_at     INTEGER NULL,
    canceled_at      INTEGER NULL
);

CREATE UNIQUE INDEX idx_pairing_links_token ON pairing_links(token_hash);
CREATE INDEX idx_pairing_links_user ON pairing_links(user_id, created_at DESC);
CREATE INDEX idx_pairing_links_open ON pairing_links(expires_at) WHERE redeemed_at IS NULL;

CREATE TABLE refresh_secrets (
    id          TEXT PRIMARY KEY,
    session_id  TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    secret_hash BLOB NOT NULL,
    created_at  INTEGER NOT NULL,
    expires_at  INTEGER NOT NULL,
    consumed_at INTEGER NULL,
    consumed_by TEXT NOT NULL DEFAULT ''
);

CREATE UNIQUE INDEX idx_refresh_secrets_hash ON refresh_secrets(secret_hash);
CREATE INDEX idx_refresh_secrets_session ON refresh_secrets(session_id, created_at DESC);
`

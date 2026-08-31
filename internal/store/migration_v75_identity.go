package store

// identityCoreV75SQL creates the identity family: the accounts, client
// instances, and credentials a backend that serves more than one screen
// has to name (docs/specs/remote-access.md §3).
//
// This is the first schema in this database that is NOT a cache. Every
// other table can be rebuilt from provider session files; these rows
// cannot. Losing them costs identity, and the recovery is re-pairing
// from a host-local surface or a recovery code — never a migration
// (§12). Nothing here may be dropped and recomputed the way a stale
// highlight-span blob is.
//
// Six tables, one migration, because they are one unit: `devices` has no
// meaning without `users`, `sessions` has none without both, and the
// three credential tables exist only to serve them. A chain that could
// stop halfway would leave a schema that describes nothing.
//
// Plural from the start. `users` holds N rows and every device, session,
// and audit row names its user explicitly, so no read anywhere resolves
// a principal by "the only one there is" (§16 phase 2, §11 hub
// deployments). The owner is a ROLE, not a count: exactly one row may
// hold it — the partial unique index below is what makes that
// structural — and the only code allowed to look a user up by role is
// the first-boot bootstrap.
//
// Timestamps are Unix milliseconds, matching every other table.
// Nullable stamps mean "has not happened": `revoked_at`, `consumed_at`,
// `disabled_at`. Go reads them as 0, which is the same answer.
//
// Three decisions worth stating because they look like omissions:
//
//   - `sessions.signing_key_id` cascades from `signing_keys`. Deleting a
//     key is the bulk form of "these credentials can never verify
//     again", so the rows it minted go with it rather than lingering as
//     sessions no presentation could ever match.
//   - `auth_audit` names its user, device, and session with plain
//     columns and NO foreign keys. The log has to outlive what it
//     describes: the record that a device was revoked is worth most
//     after that device row is gone, and a cascade would delete exactly
//     the history someone is reading.
//   - `auth_audit.event` carries no CHECK. Its value set grows every
//     phase, and SQLite cannot widen a CHECK in place — each new event
//     kind would cost a table rebuild for a log column. The closed Go
//     set (internal/identity) is the gate. `outcome` keeps its CHECK
//     because allowed/refused is the whole space and always will be.
const identityCoreV75SQL = `
CREATE TABLE users (
    id           TEXT PRIMARY KEY,
    display_name TEXT NOT NULL,
    role         TEXT NOT NULL CHECK(role IN ('owner','member')),
    created_at   INTEGER NOT NULL,
    disabled_at  INTEGER NULL
);

CREATE UNIQUE INDEX idx_users_single_owner ON users(role) WHERE role = 'owner';

CREATE TABLE devices (
    id                    TEXT PRIMARY KEY,
    user_id               TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    label                 TEXT NOT NULL,
    class                 TEXT NOT NULL CHECK(class IN ('desktop','browser','phone','cli','backend-peer')),
    platform              TEXT NOT NULL DEFAULT '',
    key_thumbprint        TEXT NULL,
    passkey_credential_id TEXT NULL,
    created_at            INTEGER NOT NULL,
    last_seen_at          INTEGER NOT NULL DEFAULT 0,
    revoked_at            INTEGER NULL
);

CREATE INDEX idx_devices_user ON devices(user_id, created_at);
CREATE UNIQUE INDEX idx_devices_key_thumbprint
    ON devices(key_thumbprint) WHERE key_thumbprint IS NOT NULL;
CREATE UNIQUE INDEX idx_devices_passkey
    ON devices(passkey_credential_id) WHERE passkey_credential_id IS NOT NULL;

CREATE TABLE signing_keys (
    id         TEXT PRIMARY KEY,
    secret     BLOB NOT NULL,
    created_at INTEGER NOT NULL
);

CREATE TABLE sessions (
    id             TEXT PRIMARY KEY,
    user_id        TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    device_id      TEXT NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    binding_class  TEXT NOT NULL CHECK(binding_class IN ('loopback-only','device-bound','public')),
    scopes         TEXT NOT NULL CHECK(json_valid(scopes)),
    signing_key_id TEXT NOT NULL REFERENCES signing_keys(id) ON DELETE CASCADE,
    created_at     INTEGER NOT NULL,
    expires_at     INTEGER NOT NULL,
    revoked_at     INTEGER NULL,
    last_seen_at   INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX idx_sessions_device ON sessions(device_id, created_at);
CREATE INDEX idx_sessions_user ON sessions(user_id, created_at);
CREATE INDEX idx_sessions_live ON sessions(expires_at) WHERE revoked_at IS NULL;

CREATE TABLE recovery_codes (
    id          TEXT PRIMARY KEY,
    user_id     TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    code_hash   BLOB NOT NULL,
    created_at  INTEGER NOT NULL,
    consumed_at INTEGER NULL,
    consumed_by TEXT NOT NULL DEFAULT ''
);

CREATE UNIQUE INDEX idx_recovery_codes_hash ON recovery_codes(code_hash);
CREATE INDEX idx_recovery_codes_unspent ON recovery_codes(user_id) WHERE consumed_at IS NULL;

CREATE TABLE auth_audit (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    at         INTEGER NOT NULL,
    event      TEXT NOT NULL,
    outcome    TEXT NOT NULL CHECK(outcome IN ('allowed','refused')),
    reason     TEXT NOT NULL DEFAULT '',
    user_id    TEXT NOT NULL DEFAULT '',
    device_id  TEXT NOT NULL DEFAULT '',
    session_id TEXT NOT NULL DEFAULT '',
    peer       TEXT NOT NULL DEFAULT '',
    detail     TEXT NOT NULL DEFAULT ''
);

CREATE INDEX idx_auth_audit_at ON auth_audit(at DESC, id DESC);
CREATE INDEX idx_auth_audit_device ON auth_audit(device_id, at DESC) WHERE device_id <> '';
CREATE INDEX idx_auth_audit_user ON auth_audit(user_id, at DESC) WHERE user_id <> '';

CREATE TRIGGER trg_auth_audit_immutable BEFORE UPDATE ON auth_audit
BEGIN
    SELECT RAISE(ABORT, 'auth_audit rows are immutable');
END;
`

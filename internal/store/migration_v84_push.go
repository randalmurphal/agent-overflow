package store

// pushV84SQL adds the two rows phone push needs: which devices have a
// registration token, and the credential this backend sends with
// (docs/specs/remote-access.md §9, "Push").
//
// AUTHORITATIVE, LIKE THE IDENTITY FAMILY IT HANGS OFF. Neither table can
// be rebuilt from provider session files. A lost token is recoverable —
// the phone re-registers on its next launch — but a lost credential is the
// owner pasting a service-account key again, so this is not cache content
// and nothing here may be dropped and recomputed.
//
// Two decisions worth stating because the alternatives look reasonable:
//
//   - `push_tokens` is keyed by DEVICE, one row each, not by token. A
//     device has exactly one live registration at a time: the platform
//     replaces the old one rather than adding a second, and a table that
//     accumulated rows per token would send the same notification twice to
//     one phone for as long as a stale row survived. Re-registering is
//     therefore an upsert, and the cascade means revoking a device takes
//     its token with it — the same "revoke a device and its state goes"
//     property §6 gives `ui_state`.
//   - `push_sender` is a SINGLETON, pinned by `CHECK(id = 1)`. One backend
//     sends as one Firebase project; a second row would be a second sender
//     nothing chooses between. The credential column holds the key file as
//     pasted, which is backend-local secret material of exactly the class
//     `signing_keys.secret` already is — it never appears on a read wire
//     shape, and `GetPushSenderStatus` answers the project and the account
//     instead.
//
// Timestamps are Unix milliseconds, matching every other table.
const pushV84SQL = `
CREATE TABLE push_tokens (
    device_id  TEXT PRIMARY KEY REFERENCES devices(id) ON DELETE CASCADE,
    platform   TEXT NOT NULL,
    token      TEXT NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE TABLE push_sender (
    id              INTEGER PRIMARY KEY CHECK(id = 1),
    project_id      TEXT NOT NULL,
    client_email    TEXT NOT NULL,
    credential_json TEXT NOT NULL,
    updated_at      INTEGER NOT NULL
);
`

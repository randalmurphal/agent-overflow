# `internal/itemmeta`

Pure, standard-library-only rewrites for the persisted `items.meta` JSON
column. Triage write paths and store migrations both use this package because
triage already imports store and the shared byte-level shaping cannot live in
either owner.

Route changes by contract:

- `trim.go`, `collab_states.go`, `collab_prompt.go`, and
  `collab_profile.go` contain fixed-point persistence trims shared with
  migration fixups.
- `transfer.go` and `transfer_codex.go` rewrite only structured ownership
  and provider identities during conversation transfer.
- `promoted_at_interrupt.go`, `provider_queued.go`, and
  `codex_runtime.go` own typed lifecycle markers and transition decoding.
- `import.go` owns import provenance and unavailable-payload markers.

Targeted rewrites preserve unknown fields, prose, unrelated provider values,
and large JSON numbers. Reject malformed metadata when a complete rewrite is
required; never replace it with an empty object and continue. Multi-key state
transitions merge in one decode/encode so no half-state can persist.

Do not add SQLite access, provider event handling, frontend projection, or a
dependency on another internal package. Provider-only interpretation remains
in its provider adapter.

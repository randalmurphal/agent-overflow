# `internal/pathlinks`

Extracts path-shaped tokens from untrusted prose, validates them against a
workspace filesystem, and returns a `PathRef` allowlist containing path,
line, and column values. Rendering and click-time open policy belong to the
frontend and `internal/editor`.

`ExtractAndValidate` and `StreamScanner` share the same bounded pipeline:
reject obvious non-paths, resolve the workspace and candidate symlinks, enforce
workspace containment, stat each unique path once, and return each valid
occurrence in source order. The containment check must occur before `os.Stat`;
otherwise link extraction could reveal whether files exist outside the
workspace. A missing, relative, non-canonical, or unreadable workspace yields
no references.

`MarshalRefsJSON` is the shared `items.meta.pathRefs` projection used by
triage and discussions. Keep `MetaKey`, `PathRef` JSON fields, candidate
bounds, and the frontend reader in
`frontend/src/lib/utils/pathLinkify.ts` synchronized.

Do not broaden this allowlist because `editor.ResolvePath` permits an explicit
clicked link outside the workspace. Passive linkification has a stricter
boundary than a user-selected destination.

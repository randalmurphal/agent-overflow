# harnessrun

Durable lifecycle ownership for harness workloads. A fresh run owns its data
root and may quarantine it after failure. A borrowed root is never removed.

The artifact registry is host-global cache state. Every destructive operation
must hold its cross-worktree lock, revalidate the canonical quarantine root,
the fresh ownership in the manifest, and the manifest checksum. Active,
leased, and pinned entries are never pruned. Tests use `t.TempDir()` for the
registry and run roots. They must not point at the real app data directory.

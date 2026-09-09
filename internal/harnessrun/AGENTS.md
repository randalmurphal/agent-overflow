# Harness run lifecycle

This package owns plans, manifests, supervision, artifacts, retention, and
retention of failed disposable harness workloads. See
[agent-harness.md](../../docs/architecture/agent-harness.md) for the surrounding
architecture.

- A fresh run owns its data root and may retain or remove it according to
  retention policy. A borrowed root is never removed.
- The artifact registry is host-global cache state. Hold its cross-worktree lock
  for every mutation.
- Before any destructive operation, revalidate the canonical retained root (`Manifest.Quarantine`),
  fresh ownership in the manifest, and the manifest checksum. Never prune an
  active, leased, or pinned entry.
- Normalize macOS `/var` and `/tmp` aliases to `/private/...` before freezing
  plan paths. Continue rejecting caller-created symlinks below those aliases.
- Tests use temporary registry and run roots. They must never point at the real
  application data directory.

Run `go test ./internal/harnessrun` after lifecycle or retention changes.

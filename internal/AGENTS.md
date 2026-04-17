# internal/

Every non-main Go package lives under `internal/`. We don't use `pkg/`.

## Package Map

| Package | Role |
|---|---|
| `provider/` | Provider process lifecycle and stdio protocols. Has its own subarea guide. |
| `triage/` | Event classification. Decides what goes to the frontend vs SQLite. |
| `store/` | SQLite access, migrations, schema. |
| `checkpoint/` | Per-turn git snapshots, turn diffs, fork/restore modes. |
| `git/` | Git operations (branches, worktrees, commit, push, PR). |
| `terminal/` | PTY session manager with ring-buffer replay. |
| `discussion/` | Multi-agent deliberation coordination. |
| `design/` | Design-mode artifact storage and reactor. |
| `attachment/` | Message attachment storage. |
| `settings/` | Persistent settings (YAML) with validation. |
| `logging/` | Structured NDJSON provider-event logging. |
| `observability/` | Opt-in OpenTelemetry tracing. |
| `workspacefiles/` | Workspace-scoped file search. |
| `testutil/` | Shared test helpers. |

## Rules

- **No cross-package state leakage.** A package exposes functions and data
  types; callers own the orchestration.
- **No circular imports.** If you feel the need, re-read the package map —
  the boundaries exist for a reason.
- **Provider-specific code stays in `provider/{claude,codex}`.** Triage and
  store are provider-agnostic.
- **No global mutable state** beyond `main.go`/`app.go` wiring. Use
  constructor-style `New*` functions and pass the result explicitly.
- **Errors bubble up.** Packages return errors; `app.go` decides how to
  surface them (user-facing state, toast, status bar). Don't log-and-swallow.

## Adding a Package

1. Confirm the responsibility isn't already covered. Extend first, add second.
2. Create `internal/<name>/` with a single-sentence purpose at the top of
   the primary file.
3. Add tests alongside (`*_test.go`). Minimum bar: any new behavior has a
   test that would fail without it.
4. Update the package map above.

## Testing Bar

- Unit tests for parsing, pure logic, data shape.
- Integration tests at `app_*_test.go` (root) for anything crossing
  package boundaries (session lifecycle, approval round-trip, git workflow).
- Tests must be deterministic. If a test needs timing, use
  `t.Setenv("TMPDIR", t.TempDir())` or per-test fixtures — never scan
  shared system state (see past flakes in git history).
- `go test ./... -count=1` must pass cleanly before any commit.

# internal/testutil/

Shared test helpers for integration tests. Helpers live here when they
are useful to more than one test file; per-test fixtures stay next to
the test that needs them.

## Layout

- `app.go` holds the mock provider-binary writers. Emits NDJSON
  (Claude) or JSON-RPC (Codex) frames from a shell script so tests can
  exercise the full provider + triage + store pipeline without a real
  CLI.
- `git.go` holds `InitGitRepo` / `RunGit` / `CanonicalPath`. Spins up a
  temp repo with an initial commit on `main`. `InitGitRepoWithOrigin`
  adds a local bare "origin" with `main` pushed and tracking, and
  `AdvanceOriginMain` pushes a commit to it through a throwaway sibling
  clone. Together they stage "the remote moved and we don't know yet",
  the fixture behind fetch / ahead-behind tests. Local paths only: no
  test here reaches a network.
- `store.go` holds the `EnsureProject` helper (threads require a
  `project_id` FK; this idempotently inserts a project row per path).

## Responsibility boundary

- What BELONGS here:
  - Helpers useful to multiple packages' tests.
  - Mock binaries that the provider packages can spawn.
- What does NOT belong here:
  - App-level test helpers that construct `*App`. Those helpers live next to
    the integration test files under package `internal/app` so they can reach
    the shell's private seams.
  - Behavior under test. Helpers stage fixtures. They don't assert.

## Notes

- `CanonicalPath` in this package intentionally duplicates
  `git.CanonicalPath`. `internal/git`'s test files import `testutil`,
  so `testutil` cannot import `internal/git` without a cycle.
  Production code uses `git.CanonicalPath` / `git.SameFilesystemPath`
  directly.

## Anti-patterns

- Do NOT import `internal/git` here. The cycle is real; `git` tests
  depend on us.
- Do NOT add App-shell helpers here. Put those next to the test file under
  `internal/app`.
- Do NOT make helpers stateful. Every helper takes `*testing.T` and a
  per-test working path.

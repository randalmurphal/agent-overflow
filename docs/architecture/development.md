# Development and builds

Read this for setup, executable bootstrap, dev watchers, generated artifacts
or packaging. `make help` lists supported commands; the Makefile and package
manifests define their current arguments and versions.

## Setup

Use the Go version required by `go.mod`, Node as declared by
`frontend/package.json`, and the pnpm version pinned in `packageManager`.
Keep the root, frontend, mobile and e2e package-manager pins identical; pnpm
switches to the pinned version before applying `--dir`.

Linux GUI builds need `libgtk-4-dev`, `libwebkitgtk-6.0-dev`, `pkg-config`
and `gcc`. Wails uses GTK4/WebKitGTK 6.0. The SQLite driver is pure Go;
syntax highlighting and platform webview glue require cgo. The release
scripts and their tests also need `git`, `zip` and `unzip`. Linux test runs
need unprivileged user namespaces
([network isolation](../specs/testing-harness.md#network-isolation)).

`make install` installs the Wails CLI and frontend dependencies, including
Chromium for the browser tests. `make dev` builds the dev bundle once and
launches it; `make dev-wsl` does the same through the Windows launcher.
Nothing watches the tree for changes: rerun the target after editing. Use the root validation
commands for completion: the Go Make targets apply platform flags and
`go-build` also compiles the `nogui` variant.

### A database from a pre-sync personal-assistant build

main is trunk and its next release ships its migrations, so when main takes
a version personal-assistant's chain already used, the personal-assistant
migrations from that version on move up one. A database a
personal-assistant build migrated before that sync holds them under the old
numbers and lacks main's table. Opening it fails with "this database was
migrated by a pre-sync personal-assistant build" and names the section
below that repairs it. Repair it with the app stopped, in
`sqlite3 <data dir>/agent-overflow.db`. A database without
`thread_pull_requests` takes [Before main's v142](#before-mains-v142) and
then [Before main's v143](#before-mains-v143), in that order; one with
`thread_pull_requests` takes only the second. The runner reads only the
highest recorded version, so the names of rows below it may not match
their numbers. `TestThePreSyncRepairsOpen` runs these blocks as written
on databases each pre-sync build left and opens the results.

#### Before main's v142

main's `thread_pull_requests` is migration v142. A database from before
that sync holds `receipt_own_turn` as v151 and has no
`thread_pull_requests`. Opening it fails at v152.

```sql
BEGIN;
CREATE TABLE thread_pull_requests (
 thread_id TEXT PRIMARY KEY REFERENCES threads(id) ON DELETE CASCADE,
 forge TEXT NOT NULL CHECK(forge IN ('github', 'gitlab')),
 host TEXT NOT NULL,
 project TEXT NOT NULL,
 number INTEGER NOT NULL CHECK(number > 0),
 url TEXT NOT NULL,
 node_id TEXT NOT NULL DEFAULT '',
 branch TEXT NOT NULL,
 worktree_path TEXT NOT NULL,
 seen_open_at INTEGER NOT NULL,
 merged_at INTEGER NOT NULL DEFAULT 0,
 handled_at INTEGER NOT NULL DEFAULT 0,
 removal_failed_at INTEGER NOT NULL DEFAULT 0,
 outcome TEXT NOT NULL DEFAULT '' CHECK(outcome IN ('', 'removed', 'kept', 'gone', 'closed', 'skipped'))
);
CREATE INDEX idx_thread_pull_requests_pending ON thread_pull_requests(forge, host, project) WHERE handled_at = 0;
INSERT INTO migration_versions (version, name) VALUES (152, 'receipt_own_turn');
COMMIT;
```

The table is `threadPullRequestsV142SQL`
(`internal/store/migration_v142_thread_pull_requests.go`). This leaves
`receipt_own_turn` at v152, which the next section moves to v153; opening
the database before that fails at v153.

#### Before main's v143

main's `codex_developer_instructions` is migration v143. A database from
before that sync holds `agent_tree_marks` as v157 and has no
`codex_developer_instructions`. Opening it fails at v158.

```sql
BEGIN;
CREATE TABLE codex_developer_instructions (
 thread_id TEXT PRIMARY KEY REFERENCES threads(id) ON DELETE CASCADE,
 session_ref TEXT NOT NULL CHECK(session_ref <> ''),
 digest TEXT NOT NULL CHECK(length(digest) = 64)
);
UPDATE migration_versions SET version = -version WHERE version BETWEEN 143 AND 157;
UPDATE migration_versions SET version = 1 - version WHERE version < 0;
INSERT INTO migration_versions (version, name) VALUES (143, 'codex_developer_instructions');
COMMIT;
```

The table is `codexDeveloperInstructionsV143SQL`
(`internal/store/migration_v143_codex_developer_instructions.go`). The two
updates move rows v143 to v157 up one; going through negative numbers
keeps the primary key unique at every row.

## Generated artifacts

- After changing `info` or `fileAssociations` in `build/config.yml`, run
  `wails3 task common:update:build-assets` to regenerate platform assets.

- Regenerate Wails bindings with `wails3 generate bindings -ts`; do not edit
  `frontend/bindings/` by hand. `build/Taskfile.yml` supplies the normal build
  flags and clean generation. Bound RPC changes also require `make methodgen`;
  see the [app guide](../../internal/app/AGENTS.md).
- `build:frontend` and `generate:bindings` in `build/Taskfile.yml` always run
  with `method: none`. Do not use Task source fingerprints for outputs
  embedded in the binary: its file walker can omit inputs after a glob or
  symlink error and leave stale output marked current.
- Keep `.claude/` and `.playwright-mcp/` explicitly excluded in
  `frontend/vite.config.ts` under `server.watch.ignored`; the Vite dev server
  runs only for `ao-harness up --dev-assets`. Git-ignore handling alone does
  not prevent recursive watches of nested checkouts.

## Bootstrap and process lifetime

- Dispatch `updater.HandleHelperMode` before ordinary CLI/session checks,
  discovery and provider setup. A paired frontend relaunches with
  `--frontend` and its data root. Do not retain a consumed invitation or
  select a computer that may have been removed. The shared updater owns argv
  preservation and rollback-environment cleanup.
- Backend and harness instance locks must be close-on-exec atomically on
  Unix. Provider and reaper children can outlive the app; inherited locks
  would block a later app instance. See
  `TestInstanceLockDoesNotSurviveInAnUnrelatedChild`.
- Publish macOS bundles through `scripts/macos-bundle.sh`. Prepare and sign a
  fresh bundle, retain any previous bundle still in use, then publish the
  replacement. Never edit or remove the bundle backing a running process.
  `TestMacOSBundleReplacementPreservesRunningCode` checks code signatures,
  publication failure and retirement. Bundle integrity does not promise
  that macOS preserves privacy grants for changing ad-hoc signatures.

## Choose the validation environment

| Change | Reference |
|---|---|
| Mocked application flows, isolated instances and browser automation | [Agent harness](agent-harness.md) |
| Sustained streaming or renderer performance | [Soak rig](soak-rig.md) |
| Provider CLI upgrade or release provider verification | `make provider-smoke`; [smoke source](../../internal/app/providersmoke_test.go) |
| Provider importer or native-history format | `make import-corpus-smoke`; [corpus smoke source](../../internal/app/importcorpussmoke_test.go) |
| Production service updater | [Artifact validation](serve-mode.md#validating-production-artifacts) |
| System Chromium upgrade or launch flags | [Browser guide](../../internal/browser/AGENTS.md) |
| Android native shell or signed candidate | [Mobile guide](../../mobile/AGENTS.md), [e2e guide](../../e2e/AGENTS.md) |
| Mobile browser access and passkey lock | [Mobile browser validation](../references/mobile-browser-testing.md) |

The provider smoke uses authenticated real CLIs and spends tokens; it needs
an explicit request. The import smoke reads only supplied copies of provider
homes and refuses overlap with live homes. Neither belongs in ordinary tests.
The system-Chromium launch and page-operation test is also manual, through
`AO_HEADLESS_CHROMIUM_SMOKE=1` and `TestHeadlessChromiumReal`; it downloads
nothing. `make verify` is the hermetic release check, including compilation
of the real-provider smoke tests.

## Continuous integration

`.github/workflows/ci.yml` runs on every pull request and push to `main`.
Every step calls a Make target, so the local command is the same check.
The jobs fill the Free plan's 20 concurrent slots where the time is: one
Linux job for `fmt-check`, `tidy-check`, `go-vet`, `go-build`, `provider-smoke-compile`
and `bindings-check`, a macOS `go-build`, one job for `go-lint` and
`go-vuln`, the frontend job (`check`, `build`, `frontend-lint`), `go-test`
in three shards by package weight (`GO_TEST_PKGS`, `GO_TEST_EXCLUDE`), the
last of which also runs `make pstack-scripts`, plus one with
`GO_TEST_FLAGS='-tags noremote'`, the
Vitest unit and browser projects as two jobs under `bin/ao-netns`, and
`make e2e` in eight Playwright shards with three workers each (`E2E_ARGS`,
`AO_E2E_WORKERS`), and `make release-upgrade-check` as `upgrade-lineage`
when the change reaches the store, the check or the version
([release upgrade check](release-upgrade-check.md)).
The Linux jobs run in the toolchain image, `ghcr.io/<repo>/ci-toolchain`,
built from `ci/Dockerfile`: Go, Node, pnpm, Bun, the GTK headers, the Playwright
browsers with their OS libraries (Chromium as the headless shell the
suites run), the Wails CLI, golangci-lint, the pnpm store and a Go module
cache holding what the tree imports under every tag set the Make targets
use. The Make test targets prefetch that same set before entering the
network namespace. A job pulls that one image instead of apt, the Go
download site, the npm registry, Bun's releases and the Playwright CDN.
`ci/toolchain-inputs.sh` lists the pins that name a tag; the `toolchain`
job (`.github/actions/toolchain-image`) resolves it at
the start of every run, builds and publishes it when it is missing and the
token may push, and otherwise runs the jobs on the newest `main` image
while `.github/actions/setup` installs what the change moved past it. Only
a run on `main` or a rebuild moves the `main` tag; a pull request's build
publishes only its own tag. A
fork or Dependabot token cannot publish, so a pin bump from one runs on
that fallback until it merges. `toolchain-image.yml` publishes on pushes to
`main` that touch a pin or a manifest and weekly, refreshing the base
image, module cache and package store. The macOS job runs on the bare
runner; the setup action installs its toolchains from the same pins.

Go build caches are saved only by pushes to `main`; a pull request restores
the newest `main` cache for its job slot, so it rebuilds and retests what
it changed. `codeql.yml` scans Go and
TypeScript; `dependabot.yml` tracks Go, npm, Gradle and action versions;
`dependency-review-action` checks pull request dependency changes.

The two lint jobs gate new issues only: `scripts/ci-change.sh` writes the
change (a pull request's merge against the base branch it merged into,
checked by `scripts/test-ci-change.sh`) and `go-lint` runs with
`--new-from-patch`, while
`frontend-lint` runs `--deny-warnings` on the changed frontend files. Both
widen to the whole tree once the existing backlog is cleared
(`make go-lint` and `make frontend-lint` show it). Branch protection
requiring these checks is a repository setting, not part of the tree.

## Releases

Use [release candidates](release-candidates.md) for producing, testing and
promoting saved artifact bytes. The candidate workflow runs the
[release upgrade check](release-upgrade-check.md) with the candidate's own
headless binary against the previous release's published corpus, and
packages the migrated corpus as the candidate's `upgrade-corpus.tar.zst`
for the next release. Only `scripts/build-release.sh` and the release
workflow set `AO_RELEASE_BUILD=1`, which stamps the frontend bundle with the
bare package version; any other build stamps a dated prerelease of the next
patch so a paired phone adopts development rebuilds in build order. Every build artifact must survive CI upload
and download before checksumming and publishing;
`TestReleaseWorkflowCarriesEveryArtifactToPackaging` checks that handoff.
For installation, pairing and Android signing, follow
[remote access setup](remote-access-setup.md).
The Windows/WSL release without remote access has its own script and feed:
[build without remote access](noremote-build.md).

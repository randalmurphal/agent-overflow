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
need unprivileged user namespaces and mounts in them
([isolation](../specs/testing-harness.md#isolation-network-temp-directory-and-priority)).

`make install` installs the Wails CLI and frontend dependencies, including
Chromium for the browser tests. `make dev` builds the dev bundle once and
launches it; `make dev-wsl` does the same through the Windows launcher.
Nothing watches the tree for changes: rerun the target after editing. Use the root validation
commands for completion: the Go Make targets apply platform flags and
`go-build` also compiles the `nogui` variant.

Every Go command under make runs with `-trimpath` (exported as `GOFLAGS` in
the Makefile), so worktrees holding the same sources share build cache
entries. A bare `go build` or `go test` compiles a second copy of every
package under a different cache key unless its own `GOFLAGS` carries
`-trimpath`. With `-trimpath`, file paths in stack
traces and from `runtime.Caller` are module-relative
(`agent-overflow/internal/...`), so a test finds the checkout with
`internal/testutil/reporoot`, and a debugger attached to a `make dev` build
needs that prefix mapped to the checkout.

Make lowers itself to nice 19, so every build and test recipe yields to
the developer's running app; goals that launch an app (`make dev`,
`make harness` and the like) keep normal priority. The test targets run
under `bin/ao-netns`, whose private temp directory and on-disk Go work
directory are described with the
[isolation](../specs/testing-harness.md#isolation-network-temp-directory-and-priority).
Go trims its build cache only by age, so `go-build`, `go-test` and
`test-race` run a `cmd/ao-gocache-trim` pass before their build or test and
another when it ends. A pass walks the cache at most every five minutes,
one at a time across sessions, and once `go env GOCACHE` passes 20 GiB
(`AO_GOCACHE_MAX_GIB`; 0 turns it off) deletes its least recently used
entries, linked executables included, never one used in the last hour;
when those alone keep the cache over the cap it says so. It resolves
`GOCACHE` by the go command's own rules and refuses a directory without
the `README` the go command writes in its cache. There a failed pass prints its error and the
target goes on, while an invalid `AO_GOCACHE_MAX_GIB` fails it;
`make gocache-trim` fails on either. It does nothing in CI.

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
Every step calls a Make target, or a binary `make harness-build` built, so
the local command is the same check. The `toolchain` job also decides which
suites a pull request runs from the paths it changes
(`scripts/ci-suites.mjs`, which holds the rules); a push or a manual run
runs every suite. The jobs are one Linux job for `fmt-check`, `tidy-check`,
`go-vet`, `go-build`, `provider-smoke-compile` and `bindings-check`, a
macOS `go-build` and `exhaustive` pass, one job for `go-lint`,
`go-lint-exhaustive` and `go-vuln`, the frontend job (`check`, `build`,
`frontend-lint`), `go-test` in three shards by package weight
(`GO_TEST_PKGS`, `GO_TEST_EXCLUDE`) plus `make go-test-noremote`, the
Vitest projects as `make vitest-unit` and `make vitest-browser`, and one
`make harness-build` whose binaries eight Playwright shards run with
`bin/ao-harness-e2e --shard=N/8` and three workers each
(`AO_E2E_WORKERS`). `upgrade-lineage` runs `scripts/release-upgrade-check.sh`
on the same binaries when the change reaches the store, the check or the
version ([release upgrade check](release-upgrade-check.md)). The `rest`
shard also runs `make script-tests`. `CI result` passes when no job failed
or was cancelled; it is the check branch protection requires, so a skipped
suite or a renamed job needs no change to that repository setting.
The Linux jobs run in the toolchain image, `ghcr.io/<repo>/ci-toolchain`,
built from `ci/Dockerfile`: Go, Node, pnpm, the GTK headers, the Playwright
browsers with their OS libraries (Chromium as the headless shell the
suites run), the Wails CLI, golangci-lint, the pnpm store and a Go module
cache holding what the tree imports under every tag set the Make targets
use. The Make test targets prefetch that same set before entering the
network namespace. A job pulls that one image instead of apt, the Go
download site, the npm registry and the Playwright CDN.
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

Every job restores its caches and only a push to `main` saves them, through
`.github/actions/cache-save` as the job's last step: the Go build cache of
its slot, cut to the entries that job used (`scripts/ci-go-cache.mjs`), and
off the image the Go module cache and the pnpm store. A pull request
restores the newest `main` entry of each slot. The Go test cache then
reports a package as `(cached)` when its code, its dependencies and the
files its tests read are unchanged; `scripts/ci-stable-mtimes.mjs` sets the
checkout's file times from content first, since Go compares a read file by
size and time. A test that lists a directory also depends on its entries'
times, so `make go-test` neither rewrites an unchanged tool in `bin/` nor
lets a Go command run `git status`, which touches `.git`; a test walks the
tree with `reporoot.Walk`, which leaves out build output, and writes nowhere
inside the checkout. The root package, `cmd/ao-netns` and
`internal/netisolate` rerun on every run: their tests read
`AO_NETISOLATE_NS`, which names the run's own network namespace.
`cache-cleanup` keeps one entry per cache name after a push
and fails when they pass 8 GB of GitHub's 10 GB
(`scripts/ci-cache-cleanup.mjs`). `codeql.yml` scans Go and
TypeScript; `dependabot.yml` opens only security updates for Go, npm,
Gradle and actions; `dependency-review-action` checks pull request
dependency changes.

The two lint jobs gate new issues only: `scripts/ci-change.sh` writes the
change (a pull request's merge against the base branch it merged into,
checked by `scripts/test-ci-change.sh`) and `go-lint` runs with
`--new-from-patch`, while
`frontend-lint` runs `--deny-warnings` on the changed frontend files. Both
widen to the whole tree once the existing backlog is cleared
(`make go-lint` and `make frontend-lint` show it). The `exhaustive` linter
has none, so the `go-lint` job also runs it alone over the whole tree
(`make go-lint-exhaustive`): a constant added to an enum fails at every
switch that does not name it. The target reaches every Go file its host
compiles in two passes, one with no build tag and one with `nogui`,
`noremote` and `providersmoke` together
(`TestExhaustiveLintPassesReachEveryFile` fails for a file no pass
compiles), then lints the Windows
launcher's packages as Windows compiles them. The macOS job runs the linter
over the default file set alone, which holds every file only macOS
compiles. A Windows-only file outside the
launcher's packages is compiled by no job and is not checked.

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

# Development and builds

Read this for setup, executable bootstrap, dev watchers, generated artifacts
or packaging. `make help` lists supported commands; the Makefile and package
manifests define their current arguments and versions.

## Setup

Use the Go version required by `go.mod`, Node as declared by
`frontend/package.json`, and the pnpm version pinned in `packageManager`.
Keep the root, frontend and mobile package-manager pins identical; Corepack
selects pnpm before applying `--dir`.

Linux GUI builds need `libgtk-4-dev`, `libwebkitgtk-6.0-dev`, `pkg-config`
and `gcc`. Wails uses GTK4/WebKitGTK 6.0. The SQLite driver is pure Go;
syntax highlighting and platform webview glue require cgo.

`make install` installs the Wails CLI and frontend dependencies, including
Chromium for the browser tests. `make dev` starts the local dev supervisor;
`make dev-wsl` runs the Windows launcher path. Use the root validation
commands for completion: the Go Make targets apply platform flags and
`go-build` also compiles the `nogui` variant.

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
- Keep `.claude/` and `.playwright-mcp/` explicitly excluded in both
  `build/config.yml` under `dev_mode.ignore.dir` and
  `frontend/vite.config.ts` under `server.watch.ignored`. Git-ignore handling
  alone does not prevent recursive watches of nested checkouts.

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

The provider smoke uses authenticated real CLIs and spends tokens; it needs
an explicit request. The import smoke reads only supplied copies of provider
homes and refuses overlap with live homes. Neither belongs in ordinary tests.
The system-Chromium launch test is also manual, through
`AO_HEADLESS_CHROMIUM_SMOKE=1` and `TestHeadlessChromiumReal`; it downloads
nothing. `make verify` is the hermetic release check, including compilation
of the real-provider smoke tests.

## Releases

Use [release candidates](release-candidates.md) for producing, testing and
promoting saved artifact bytes. Every build artifact must survive CI upload
and download before checksumming and publishing;
`TestReleaseWorkflowCarriesEveryArtifactToPackaging` checks that handoff.
For installation, pairing and Android signing, follow
[remote access setup](remote-access-setup.md).

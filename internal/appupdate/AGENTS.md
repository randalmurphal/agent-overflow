# internal/appupdate/

Owns the backend's in-app update state machine: GitHub release discovery,
specific-version selection, checksum verification, download fencing, WSL
staging and launcher handoff, install deadlines, and update lifecycle events.

The package is tag-free. It may import `wails/v3/pkg/updater`, which is the
framework-independent updater library, but it must not import
`wails/v3/pkg/application`. Native Wails application wiring stays in the
`internal/app/app_updater_desktop.go` adapter behind `!nogui`.

## Boundaries

- `Service` owns all updater mutexes, timers, provider handles, pending/staged
  releases, and WSL install state. Do not split that live state back across
  `internal/app.App` fields.
- `internal/app.App` retains the five Wails/transport binding methods and DTOs.
  Their names, signatures, declaring type, and JSON fields are wire contracts.
- The application-shell boot adapters resolve link-stamped globals, environment-derived WSL
  paths, process shutdown, notifications, and Wails application events. Pass
  only narrow callbacks or explicit configuration into this package.
- `internal/selfupdate` remains the filesystem and cross-process contract used
  by both the backend and Windows launcher. Launcher-owned wire constants,
  marker primitives, staging copies, and the local-file provider belong there;
  backend lifecycle policy belongs here.
- Update lifecycle events must flow through `eventchan`; do not add a direct
  transport or Wails application dependency.

## Asset names are matched exactly

`matchReleaseAsset` (`assetmatch.go`) replaces the updater library's
`DefaultAssetMatcher`, which accepts any asset whose name CONTAINS the platform
and arch tokens and returns the first one it finds. That is safe only while
every artifact in a release happens to be disjoint under substring matching,
which is a property of today's list rather than a rule. Adding
`agent-overflow-headless-linux-amd64` broke it: the name contains "linux" and
"amd64", sorts ahead of `agent-overflow-linux-amd64`, and every Linux desktop
install would have taken the windowless serve binary as its next update and
then opened no window, with nothing reporting a mismatch.

So an asset is a target's iff its name is `agent-overflow-<platform>-<arch>`
plus one of `releaseAssetExtensions`. Consequences for anyone editing here:

- A NEW release artifact is named with its qualifier in the middle
  (`agent-overflow-<qualifier>-<platform>-<arch>`), which is what keeps it out
  of every target's match.
- A new artifact EXTENSION is added to `releaseAssetExtensions` deliberately.
  An unlisted one is refused, not guessed at.
- `platform` is not `runtime.GOOS`. The WSL backend targets `wsl`.
- Both halves of the provider pair take the matcher from
  `newGitHubProvider`, and fixtures build providers through it too, so a test
  cannot exercise a matcher the app does not ship.

`TestReleaseAssetMatcherAgreesWithTheReleaseScript` reads the artifact names
out of `scripts/build-release.sh` and fails if any target resolves to the wrong
one, to nothing, or to an artifact another target also claims — so a colliding
name fails at the commit that adds it rather than in somebody's install.

## Verification

Tests use mock GitHub servers and fake hosts. They must never contact GitHub or
spawn a real provider process. Cover state transitions and failure paths,
especially checksum refusal, concurrent-operation fences, WSL marker cleanup,
ACK/backstop races, and event ordering around fence release.

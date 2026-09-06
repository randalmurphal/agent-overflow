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
- `internal/app.App` retains the five Wails/transport binding methods. Shared
  wire DTOs live here, with App aliases; the independent desktop frontend uses
  the same service and shapes. Names, method IDs and JSON fields remain stable.
- The application-shell boot adapters resolve link-stamped globals, environment-derived WSL
  paths, process shutdown, notifications, and Wails application events. Pass
  only narrow callbacks or explicit configuration into this package.
- `internal/selfupdate` remains the filesystem and cross-process contract used
  by both the backend and Windows launcher. Launcher-owned wire constants,
  marker primitives, staging copies, and the local-file provider belong there;
  backend lifecycle policy belongs here.
- Update lifecycle events must flow through `eventchan`; do not add a direct
  transport or Wails application dependency.
- `Config.RelaunchArgs` is forwarded to the shared updater unchanged. Nil
  preserves launch arguments; a non-nil slice replaces them. A paired frontend
  normalizes consumed invitations to `--frontend`. The executable dispatches
  `updater.HandleHelperMode` before CLI/session guards and normal boot.

## `ReleaseSource` is the chain without a handle

Every other surface here hangs off `*updater.Updater`, and a supervised `serve`
host has none: no `application.App` to build one against, and no use for the
framework's install half, because a serve host is updated by its supervisor
rather than by swapping its own file (`docs/architecture/serve-mode.md`). What
it does need is the half above the install — enumerate releases, resolve one,
download it verified — so `source.go` exposes exactly that: `List`, `Latest`,
`Fetch`.

`Configure` is built ON `NewReleaseSource` rather than beside it. That is the
whole point of the type: one constructor means the desktop updater and a serve
host's source share one `targetableProvider` (one matcher, one by-tag resolve,
one listing) and one `verifiedProvider` (one fail-closed refusal of a release
that ships nothing to verify against). Two constructors would be two answers to
"which asset is this host's", and the wrong one is the one nobody is looking at.

Things to keep true when editing here:

- **`Fetch` hashes the stream itself.** `verifiedProvider.Download` does not
  verify — on the desktop path the *Updater* computes the digest during download
  and `verify()` compares it. There is no Updater here, so `Fetch` streams
  through a `sha256` beside `dst` and refuses a mismatch. Removing that
  comparison would leave the whole path unverified while still reading as if it
  were checked.
- **Downloads are bounded before writing.** `verifiedProvider.Download` caps
  reported and streamed artifact bytes at 2 GiB for both desktop and supervised
  updates. Missing or dishonest lengths cannot fill the disk until a timeout.
  Extraction has its own file-count and expanded-size limits.
- **The caller owns `dst`.** A failed `Fetch` may already have written part of
  an artifact; discarding it is the caller's job (`internal/app`'s flow
  downloads into a temp file it removes on every exit path).
- **`Platform` is refused when empty.** It is the artifact token, not
  `runtime.GOOS`: a serve host targets `headless-<GOOS>`, the WSL backend
  targets `wsl`. An empty token matches no asset in any release and would read
  as "there are no updates" rather than as the configuration mistake it is.
- **`Latest` reads the LISTING**, not `/releases/latest`: the listing is already
  filtered to what this host can install and is the only answer carrying a tag.
  A "latest" that ships no asset for this platform is a release for somebody
  else, not an update.

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

# internal/appidentity/

Every per-instance name the desktop binary and the WSL launcher use, all
derived from one mode string. Pure and dependency-free, so the two entry points
cannot drift on single-instance ids, window titles, or diagnostic paths.

## The mode axis

`ModeDev` / `ModeProd` are build stamps. `ModeHarness` / `ModeSoak` /
`ModePerf` are RUNTIME modes the same binary enters when the operator
passes `--profile` (or `AGENT_OVERFLOW_PROFILE`). None of the three may
ever become a build stamp: such a build would be indistinguishable from
the dev build the developer does real work in.

`LauncherMode(buildMode, profile)` folds the two into the one string every
helper branches on. The profile wins, so an isolated instance launched
from a dev build is that profile, never `dev`.

## Rules

- An unknown profile is an error, never a fallback. `NormalizeProfile`
  accepts only `""`, `harness`, `soak`, and `perf`. A typo that quietly
  resolved to the default would point an isolated instance at the
  developer's own state, which is what the axis exists to prevent.
- Per-instance names derive from the folded mode and nothing else:
  `SingleInstanceID`, `AppTitle`, `WebviewProfileDir`,
  `RenderForensicsDir`, `DevToolsPort`. `StateFileName` is the one
  exception, suffixing only for isolated profiles (`launcher-soak.log`,
  `window-perf.json`), because a developer expects one `launcher.log` and
  one remembered window placement across dev and prod.
- `DevToolsPort` is distinct per diagnostic mode (dev 9223, soak 9224,
  harness 9225, perf 9226) and 0 for production, where CDP is
  unauthenticated. Every diagnostic instance can be up at once, and two
  WebView2s asked for one port leave whichever lost the bind unattachable.
- Adding a mode means updating `isolatedMode` plus every switch in
  `profile.go` and `singleinstance.go`. A mode missing from one of them
  answers with the developer's own name instead of failing.

`cmd/agent-overflow-windows/AGENTS.md` § CLI flags is the consumer side: what
each profile boots and which Make target drives it.

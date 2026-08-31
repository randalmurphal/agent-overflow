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

## Verification

Tests use mock GitHub servers and fake hosts. They must never contact GitHub or
spawn a real provider process. Cover state transitions and failure paths,
especially checksum refusal, concurrent-operation fences, WSL marker cleanup,
ACK/backstop races, and event ordering around fence release.

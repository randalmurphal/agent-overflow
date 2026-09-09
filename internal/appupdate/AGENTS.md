# Application update service

This package owns release discovery, exact artifact selection, verified
downloads, update serialization, WSL staging and launcher handoff, deadlines, and
lifecycle events. It may use the framework-independent updater library but must
not import the Wails application package.

Service owns its mutexes, timers, provider handles, pending releases, and WSL
install state. Application adapters supply narrow platform callbacks. Stable
binding DTOs remain here, and lifecycle events flow through eventchan.

RelaunchArgs passes unchanged to the shared updater: nil preserves original
arguments and a non-nil slice replaces them. The paired frontend normalizes
consumed invitations before relaunch. Executables dispatch updater helper mode
before ordinary boot.

ReleaseSource provides listing, resolution, and verified fetches for supervised
hosts that do not own an in-process updater. Keep desktop and supervised paths
on the same provider and exact matcher.

Artifact ownership is exact:
agent-overflow-<platform>-<arch> plus an approved extension. Platform is the
release target token, such as wsl or headless-<os>, rather than runtime.GOOS.
Reject an empty target, unknown extension, collision, or release with no
checksummed artifact. Latest means the newest listing entry installable by this
target.

Fetch streams through SHA-256 while writing and rejects mismatches. Cap reported
and streamed download size before disk growth. The caller owns cleanup of a
partially written destination and any platform-specific archive handling.

Tests use mock release servers and fake hosts. Cover checksum refusal, asset
collisions, size limits, concurrent-operation exclusion, WSL marker cleanup,
acknowledgement and timeout races, relaunch argv, and event ordering. The release
script drift test must continue to prove that every produced artifact has
exactly one target.

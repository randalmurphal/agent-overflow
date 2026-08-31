# Build Assets

This directory contains the supported desktop build assets for Agent Overflow.

- `darwin/` holds the macOS `.app` bundle metadata and icons.
- `linux/` holds the Linux desktop entry and optional package metadata.
- `windows/` holds the Windows WSL launcher icon, manifest, and version info.
- `docker/Dockerfile.cross` builds the optional cross-compilation image.

The first direct release supports native Linux, macOS `.app`, and Windows via
the WSL launcher. macOS artifacts are ad-hoc signed; the updater's SHA-256
sidecar remains their integrity check. Android, iOS, native Windows installers,
and server Docker packaging are intentionally not present.

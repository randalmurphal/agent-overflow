# External editor integration

This package discovers supported editors and opens files, directories, or
line-and-column locations. Keep editor-specific command construction here and
keep UI selection and settings persistence in their owning packages.

Detection must prove that a candidate can be invoked, not merely that a
similarly named file exists. Resolve explicit settings first, then platform
install locations and PATH. Preserve deterministic priority, distinguish a
missing editor from a broken probe, and return diagnostics without turning one
bad candidate into failure of every candidate.

macOS application bundles, Windows executable and shim forms, Linux desktop
launchers, WSL interop paths, and remote editor CLIs have different invocation
contracts. Keep those branches in platform-specific files. Build argv directly;
paths may contain spaces, Unicode, leading dashes, and shell metacharacters.
Never assemble a shell command.

Shim validation follows the final executable and rejects scripts or wrappers
that would detach incorrectly unless that editor's integration explicitly
supports them. Fast-exit observation distinguishes a successful handoff from an
immediate launch failure, while allowing editors that intentionally detach.
Always reap the observer process and bound the wait.

Any package-level detection cache or test hook must be restored after tests and
safe across repeated and concurrent calls. Adding an editor includes detection,
stable identity, display metadata, invocation rules for every supported
platform, and table tests for files, folders, and positions.

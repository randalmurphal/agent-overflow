# Self-update filesystem contract

This package contains framework-free update shapes and filesystem operations
shared by the backend, launcher, and updater helper. Update policy and release
selection belong in internal/appupdate.

InstallDirective validates a bare filename and never accepts a path. Staged file
providers confine reads to their owned directory, reject symlinks and reparse
points where relevant, and preserve digest verification in the installing
updater. Marker and handoff files use private, atomic persistence.

Do not add Wails application dependencies or network release discovery here.
Tests cover traversal, separator variants, links, replacement interruption, and
cross-process serialization.

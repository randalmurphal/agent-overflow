# Background service installation

This package renders and installs per-user service definitions for supported
platforms. Service process behavior belongs to the root executable and
supervisor.

Generated units preserve the absolute executable, data root, required mode, and
restart policy without shell interpolation. Quote according to the target
service manager, not a shell. `Install` writes the unit, reloads the user
service manager, and enables and starts the service. `Uninstall`, `Stop`, and
`Start` retain their distinct service-manager semantics.

Keep system-wide installation and privilege escalation out of this package.
Tests compare complete generated definitions and use temporary roots; they do
not call the developer's service manager.

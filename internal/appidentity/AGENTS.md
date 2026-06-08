# internal/appidentity/

Shared process identity and display-name helpers for Wails application instances.

Keep this package pure and dependency-free. It exists so native desktop
and WSL launcher entry points cannot drift on dev/prod single-instance
IDs or app titles.

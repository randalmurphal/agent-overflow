# internal/appdirs/

The one fallback chain locating the app-managed directory root
(`os.UserConfigDir()` → `os.UserHomeDir()`, then `/agent-overflow`).
`main.go`'s boot-time settings reads and the offline `ao` CLI resolve
through here so they can never drift from the directory the App uses.

Keep this package free of flags/overrides — callers own `--data-dir` /
`--config-root` semantics and how a resolution failure is treated.

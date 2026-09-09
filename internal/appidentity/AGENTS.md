# Application and device identity

This package owns two related sets of names:

- validated runtime profiles and the per-instance launcher, storage,
  diagnostics, browser, and CDP names derived from them; and
- the installation's mutable display name, including validation, atomic
  persistence, cached reads, and host-name fallback.

`NormalizeProfile` accepts empty, `harness`, `soak`, and `perf`. An isolated
profile overrides the build mode in `LauncherMode`; runtime profiles never
become build stamps. Unknown values fail rather than falling back to developer
state. Adding a mode requires updating every exhaustive naming switch.

Diagnostic modes must coexist without sharing browser storage or ports.
Production exposes no CDP port. Derive single-instance IDs, titles, WSL payload
directories, state filenames, WebView profiles, browser profiles, diagnostic
paths, and CDP ports through these helpers.

`DeviceName` is display metadata, not a stable device ID or credential.
`NormalizeDeviceName` enforces the wire limit and rejects invalid UTF-8 and
control characters. An empty saved name falls back to `HostDisplayName`.
`Get` notices external file changes; `Set` uses `atomicfile` and invalidates
the local cache.

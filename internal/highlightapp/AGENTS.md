# internal/highlightapp/

Application coordination around the pure `internal/highlight` parser.

`Service` owns the content-addressed cache, streaming fence seed state, bounded
diff-persistence workers, persisted span encoding, and remote-client gating.
`internal/app` injects lifecycle state, diff-context resolution, filesystem
reads, event emission, and the store; Wails DTOs and methods remain on the named
`main.App` wrapper through promoted methods.

This package holds no thread-to-directory lookup. `PatchWithContext` and
`ObserveDiffPayload` take an already-resolved workspace directory; the App
resolves it per scope (`gitapp.ResolveWorkspace` for a checkout,
`threadDiffWorkspace` for the edits scope) before calling in. Never reintroduce
a `WorkspaceForThread`-style closure: a workspace path reaching this package
un-resolved is a trust-boundary bypass.

Provider text and patches are bounded before parsing. Invalid UTF-8 and
incomplete parses never cross a content-addressed persistence/event boundary.
Dropped work always degrades to the ordinary RPC path.

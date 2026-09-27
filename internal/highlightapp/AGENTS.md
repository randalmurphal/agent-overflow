# internal/highlightapp/

Application coordination around the pure `internal/highlight` parser.

`Service` owns the content-addressed cache, live highlighting of streaming
code blocks, bounded diff-persistence workers, and persisted span encoding.
`internal/app` injects lifecycle state, diff-context resolution, filesystem
reads, event emission, and the store. Stateless code/patch request and result
DTOs live in `wire.go`, shared by the App wrapper and frontend-only controller;
contextual methods remain on the App, where workspace ownership is resolved.

This package holds no thread-to-directory lookup. `PatchWithContext` and
`ObserveDiffPayload` take an already-resolved workspace directory; the App
resolves it per scope (`gitapp.ResolveWorkspace` for a checkout,
`threadDiffWorkspace` for the edits scope) before calling in. Never reintroduce
a `WorkspaceForThread`-style closure: callers must resolve a workspace path
before passing it here, or they could bypass workspace ownership checks.

Provider text and patches are bounded before parsing. Invalid UTF-8 and
incomplete parses never cross a content-addressed persistence/event boundary.
Dropped work always degrades to the ordinary RPC path.

Live highlighting (`live.go`) follows each streaming assistant row's open
fence with one incremental `highlight.Stream`, keeps its tree only while the
fence streams and within `liveTreeBudget`, and pushes numbered line-range
deltas on `highlight:live`. The router observes assistant text before it
emits it, and the observer announces each fence with a language
synchronously (seq 1: line hashes and `Head`, no spans), so a client learns
of the fence before its text arrives and does not request spans for it.
Every fence it follows ends with a final push:
spans from the stateless path, or a stop (a cap, invalid text, a failed
parse, a purged row), so a client never waits on a fence the service
dropped. The client side is `liveCodeSpans.svelte.ts`.

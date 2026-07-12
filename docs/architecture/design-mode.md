# Design Mode

Design mode gives an agent an isolated, per-thread working directory for visual
HTML/CSS/JS exploration. The active design is served into a sandboxed preview
iframe beside the chat, while the conversation remains the history of the
iteration. Implementation lives in `internal/design/`, `app_design.go`, and
`frontend/src/lib/components/design/`.

## Thread and Session Model

`design` is a thread type selected at creation. It cannot be assigned through
`UpdateThreadMode`; only ordinary `chat` and `plan` threads can switch mode after
creation.

Before a design provider session starts, the app:

1. Ensures the thread's workdir exists.
2. Loads the bundled design system prompt, using
   `<configDir>/prompts/design-mode.md` as an override when present and appending
   read-only project-reference guidance when the thread has a workspace path.
3. Sets the provider process's working directory to the design thread directory
   instead of the project workspace.
4. After stopping any previous session, starts the file watcher, registers the
   thread with the design MCP server, and primes screenshot support in the
   background.

Both Claude and Codex receive the same design prompt and tokenized HTTP MCP
server. Claude receives the server through `--mcp-config`; Codex receives it in
the session's `config.mcp_servers` override. Provider-specific packages only
translate the common session configuration.

## Working Directory

Design files live below the app database directory:

```text
design-workdirs/
└── <thread-id>/
    ├── main/
    │   └── index.html
    └── options/
        └── <set-id>/
            ├── .picked
            └── <option-id>/
                └── index.html
```

`main/` is the active design. `EnsureThread` creates both top-level directories
and seeds `main/index.html` with a waiting page when it is absent. The workdir is
the design's current state; there is no snapshot or artifact history stored in
SQLite.

The agent may create component-level alternatives under
`options/<set-id>/<option-id>/`. An option is exposed to the UI only after its
directory contains `index.html`. Picking an option writes an idempotent
`.picked` marker for the set; the option files remain available so the agent can
read the chosen direction and apply it to `main/`. On reload, the backend
reconstructs the picker from the most recently modified unpicked set.

Thread, set, and option path segments reject empty values, traversal segments,
and path separators. Deleting a thread wipes its complete design directory.

## Preview and File Changes

The transport mounts the design file handler at `/design/` and restricts it to
loopback clients, including when the rest of the app is LAN-bound. HTML
responses are parsed and receive a diagnostic and capture script at the start of
`<head>`; non-HTML files pass through unchanged.

The preview is an optional `design-preview` companion pane opened from the
design thread's header. Its iframe loads
`/design/<thread-id>/main/?cb=<revision>` with
`sandbox="allow-scripts"` and no `allow-same-origin`. The toolbar selects a
375-pixel mobile, 768-pixel tablet, or container-width desktop viewport, forces
a refresh, and can hand the current design to a new chat thread.

A recursive watcher observes the thread's `main/` and `options/` trees. It
coalesces save bursts, ignores atomic-write `.tmp` paths, and emits:

| Subject | Backend event | Frontend effect |
|---|---|---|
| `main/` | `design:reload-main` | Cache-bust and reload the matching preview iframe. |
| `options/<set-id>/` | `design:options-update` | Re-read the latest unresolved option set and show its iframe grid. |

If recursive watch installation fails, the watcher falls back to periodic main
reload events. The frontend adds its own per-thread throttles before reloading
the iframe or refreshing options.

## Diagnostics and Screenshots

The injected script captures `console.error`, `console.warn`, uncaught window
errors, and unhandled promise rejections. It sends batches to the parent with
`postMessage`; the preview accepts messages only from its own iframe and
forwards them through `IngestDiagnosticBatch`.

Diagnostics are held in a bounded 100-entry ring per thread. The backend assigns
monotonic tokens, and agents request only entries newer than a supplied token.
After a watched file change, an empty read may wait briefly for the iframe to
reload and report, avoiding a stale-empty result. Session teardown drops the
thread's ring and wakes pending reads.

The loopback MCP server is started lazily on the first design-thread
registration. A per-thread URL token selects the thread without exposing a
thread-id tool argument. It offers two tools:

| Tool | Result |
|---|---|
| `get_design_diagnostics(since_token)` | New diagnostics and the next monotonic token. |
| `read_screenshot()` | JPEG tiles of the live main design, ordered top-to-bottom. |

`read_screenshot` does not use the visible iframe. A shared headless Chromium
manager loads the same `/design/<thread-id>/main/` URL, captures a full-page
PNG, and slices it into at most eight JPEG tiles. If content exceeds that
budget, the MCP response adds a clipping note. Capture failures and cancellations
are returned as MCP tool errors without terminating the MCP session.

The iframe has a separate single-PNG capture path for the user-facing handoff.
The injected script lazy-loads the embedded `modern-screenshot` bundle, captures
the document, and returns the PNG to the parent over `postMessage`.

## Clarifications and Options

The design prompt defines structured messages in fenced `aoflow-design` JSON
blocks. Assistant `clarification_request` blocks are parsed from streamed
assistant text and rendered as multiple-choice controls in the chat column.
Submitting the picker sends a normal user message containing a
`clarification_response` block.

Options are file-driven rather than declared through an MCP tool. When the
watcher reports an option set, the companion pane replaces the main preview with
a grid of sandboxed option iframes. Picking one sends a normal user message with
an `option_chosen` block containing the set, option, and relative path, then
marks the set picked on disk. The agent reads that message on its next turn and
applies the selected direction to `main/`.

## Send to Thread

`Send to thread` creates a new `chat` thread in the same project, carrying the
source thread's provider, model, runtime, workspace, worktree, and branch
settings. The new draft contains the absolute `main/` path and a bounded manifest
of its top-level regular files. A best-effort iframe capture is uploaded as a PNG
attachment when available; capture failure does not prevent the handoff.

The draft is saved but not sent, so the user can add the implementation request.
If attachment upload or draft persistence fails after thread creation, the new
thread is deleted to clean up the partial handoff.

## Lifecycle and Ownership

The app owns one watcher and one MCP registration per active design session.
Stopping or replacing the session stops the watcher, removes the diagnostic
state, and unregisters the thread token. Application shutdown tears down design
sessions, closes screenshot support, and then closes the design MCP server.

SQLite continues to store the ordinary thread and conversation items. Design
file bytes and option-pick markers live in the per-thread workdir. Viewport
selection is pane-local frontend state; clarification and option projections are
reconstructed from conversation items and the workdir, respectively.

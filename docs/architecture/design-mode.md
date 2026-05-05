# Design Mode

Design mode is a sandbox for visual UI exploration. The agent generates
self-contained HTML/CSS/JS artifacts that render in a sandboxed iframe alongside
the chat — no project file writes, no live preview of the dev server. The mode
is for sketching: hero sections, settings panels, dialog mockups, layouts a
human will later implement (or hand off to a chat thread to implement).

Implementation lives in `internal/design/`,
`frontend/src/lib/components/design/`, the Claude tool dispatcher in
`app_claude_design.go`, and the per-thread MCP server for Codex in
`internal/provider/codex/mcp.go`.

## Mode as Thread Type

Threads are typed at creation. The `mode` column on `threads` carries the type:

| `mode` value | Meaning |
|---|---|
| `chat` | Standard coding-agent conversation. Composer carries the chat/plan agent-mode toggle. |
| `design` | Sandbox-iframe design exploration. Composer hides the chat/plan toggle; the agent receives the design system prompt and the `render_design` / `present_options` tools. |
| `discussion` | Multi-agent deliberation. See [`discussion-deliberation.md`](discussion-deliberation.md). Out of scope here. |

**Thread type is immutable after creation.** Existing code treats `mode` as
mutable (`ModeCycleButton.svelte` cycles `chat → plan → design`); the rewrite
removes that cycle and folds `plan` back into a within-chat agent toggle.
Switching the *type* of an existing thread mid-conversation is not supported.

The "active mode" of the UI is derived from the active thread's type — there is
no separate workspace-mode state. Clicking a thread in the sidebar switches the
top tab to match.

## Top-Level Mode Tab

A two-button segmented control (`Chat | Design`) sits centered in the
`ChatHeader` row, between the title/project chip cluster on the left and the
action cluster (open-in-editor, terminal, diff) on the right. The chat header
uses a 3-column grid (`1fr auto 1fr`) so the tabs auto-center regardless of the
left/right cluster widths.

Clicking a tab:

- If a thread of that type is loaded, stay on it (no-op).
- Else load the most recent thread of that type from the active project, or
  create a new one if none exist.

The existing thread-creation button is unchanged. New threads inherit the type
of the currently-active top tab.

## Sidebar

All threads (chat + design) appear intermixed in the existing project rail.
Design threads carry a small monochrome `frame` icon at the right of the row —
quiet, not loud. Chat threads carry no icon. Clicking any thread switches the
top tab to match its type.

The current `ThreadRowBadges` "Dsn" pill is replaced by the icon. The icon is
defined in `ThreadRowBadges.svelte` and renders only when `thread.mode ===
'design'`.

## Design Pane Layout

When a design thread is active the body splits horizontally:

| Pane | Default | Min | Notes |
|---|---|---|---|
| Chat (left) | 45% | 320px | Timeline + composer. Composer hides the chat/plan toggle and replaces it with a small `Design` lock pill. |
| Preview (right) | 55% | 400px | Toolbar + sandbox iframe. |

The split is horizontally resizable via the existing
`SidebarResizer`-style drag handle. In chat-mode threads the right pane is not
mounted at all — the chat surface owns the full width as today.

Preview-pane toolbar (left to right):

- **Viewport selector** — three icon-only buttons (mobile / tablet / desktop)
  driven by `pane.designViewport`. No size labels.
- **Refresh** — re-fetches and re-renders the active artifact.
- **Annotate** — toggles comment mode (see § Comment Mode).
- **Artifact dropdown** — `v12 · 3m · Hero with sticky nav`. Opens a menu of
  prior artifacts in this thread, newest first, labelled
  `v{n} · {relative time} · {agent-given title}`. Defaults to the latest. No
  thumbnail strip.
- **Send to chat…** (right cluster) — opens a menu with three handoff options:
  - `Bundle (HTML + summary + PNG)` — recommended; multimodal models read both
    source and image.
  - `HTML + summary`.
  - `PNG render only`.

  Each option creates a new chat thread under the same project, seeds the draft
  with the artifact reference, and switches to it (the existing
  `exportToNewThread` flow generalises to take a content-shape argument).

Below the toolbar: the iframe, sandboxed `allow-scripts` only,
`srcdoc`-rendered from the artifact HTML on disk.

## Empty State

When the user enters design mode and no design thread exists in the current
project, the right pane shows:

> **Describe what you want to build**
>
> Renders, screen variations, and any options the agent surfaces will appear
> in this thread.

Plus a row of example prompt chips. The composer is focused; the first message
creates the thread.

The chat pane in this state shows only the composer — no duplicate empty-state
copy in the timeline.

## Artifact Storage

Unchanged from the current implementation. `internal/store/designs.go` owns the
`design_artifacts` table:

| Column | Notes |
|---|---|
| `id` | Artifact UUID. |
| `thread_id` | Owning thread. |
| `title` / `description` | Agent-supplied. |
| `kind` | Reserved; currently always `html`. |
| `html_path` | File path under `<dbDir>/design-artifacts/<threadId>/<id>.html`. |
| `created_at` | Used for the dropdown's relative-time label. |

**New:** every `render_design` event triggers a sibling `<id>.png` capture of
the rendered HTML at desktop viewport (1280px) using the existing
`captureHtmlToPng` helper. The PNG drives:

- Future thumbnail rendering if we want it.
- The "Bundle" / "PNG render only" handoff (no per-export capture round-trip).

Capture is **frontend-driven** because `captureHtmlToPng` needs a real DOM
(modern-screenshot in a hidden iframe) — the Go process has no rendering
engine. The flow:

1. Backend persists the HTML artifact and emits `design:artifact`.
2. The frontend handler in `events.ts` listens for that event and fires
   an asynchronous capture via `captureHtmlToPng(html, {width:1280})`.
3. The base64 PNG is uploaded to the backend via the new
   `SaveDesignArtifactPng(threadID, artifactID, b64)` binding, which
   calls `ArtifactStore.SavePNG` and writes `<id>.png` next to the HTML
   atomically.
4. Subsequent "Send to chat" handoffs read the persisted PNG via
   `GetDesignArtifactPng` and fall back to a fresh on-demand capture
   only if the persisted file is missing.

The capture is best-effort: failures don't block the UI, and the export
flow has its own fallback path.

## Tools

The agent calls one of two tools while in design mode:

- `render_design(title, description, html)` — produces a new artifact. The
  response includes the `artifact_id`. The frontend appends the new artifact
  to `pane.designArtifacts` via the existing `design:artifact` event.
- `present_options(prompt, options[])` — surfaces N candidate mockups. Blocks
  the agent until the user picks one. The chosen option's `artifactId` flows
  back to the model as a structured tool result.

Both tools are dispatched through a single shared path (see § Backend
Cleanup). The current Claude-vs-Codex fork is removed.

## Backend Cleanup

The current implementation has four issues that the rewrite addresses:

1. **Forked tool dispatch.** Claude routes through provider events watched in
   `app_claude_design.go`; Codex routes through a per-thread HTTP MCP server in
   `internal/provider/codex/mcp.go`. Same two tools, two implementations. The
   rewrite collapses both into the design package, which exposes a single
   `Reactor.Dispatch(threadID, toolName, args) → toolResult` entry point.
   Provider adapters call into it; the Codex MCP server becomes a thin
   translator.
2. **Choice writeback for Claude is still synthetic.** `runClaudeDesignTool`
   feeds the user's choice back to Claude via `sendMessage` with the
   `'The user chose design option "X"…'` string. Replacing this with a
   real tool-result block requires registering the design tools as an MCP
   server in the Claude session config (parity with the Codex path). The
   Claude provider doesn't currently have MCP-server support — that's a
   bigger cross-cutting refactor in `internal/provider/claude/`. For now
   the synthetic writeback ships, with a clear TODO comment in
   `app_claude_design.go`. **Follow-up task**: add MCP support to Claude,
   then route both providers through the design MCP server uniformly.
3. **Pending-options concurrency.** The reactor's `pending` map is already
   keyed by `requestID` (UUID per call), so concurrent `present_options`
   from the same thread no longer clobber each other on the backend.
   The frontend `pane.pendingDesignOptions` slot still holds a single
   request at a time — that's a UI affordance (the picker dialog only
   has one slot) and ranks below the backend correctness fix.
4. **Hardcoded 30-min timeout.** Drop the `time.After(30*time.Minute)` race in
   `app_claude_design.go`. The reactor blocks until the user chooses or the
   provider session closes. If a user wants to abandon a pending choice,
   they can use the existing thread-cancel path.

## Comment Mode (Deferred)

A future feature for in-iframe annotations. The interaction model is locked
in; implementation is deferred.

**Behaviour:**

1. The Annotate toolbar button toggles comment mode.
2. While active, hovering over the iframe outlines the element under the
   cursor with the agent-overflow accent color and a small selector label
   (e.g. `section.metrics`).
3. Clicking pins a numbered comment marker on that element and opens an
   inline input.
4. Pinned comments stack as numbered overlays on the iframe.
5. A dock at the bottom of the preview pane lists all pending comments with
   their target selectors and text. A `Send N to chat` button packages the
   set into a structured user message that gets injected into the design
   thread (or, optionally, exported to a new chat thread).

**Sandbox & granularity:**

- The iframe stays `sandbox="allow-scripts"` with no `allow-same-origin`
  escape. A small probe script is injected into the artifact HTML at save
  time when comment mode might be used; the parent enables it via
  `postMessage`.
- The probe `postMessage`s `{type:'hover'|'click', selector, rect, snippet}`
  back to the parent.
- The probe walks up from `event.target` until it finds an element matching
  one of: a semantic tag (`header / nav / section / article / aside /
  footer / figure / main`), a block-level element with explicit padding /
  margin / border / background, or an interactive primitive (`a / button /
  input`). Pure wrapper divs (single child, no styling beyond layout) are
  skipped.
- `Cmd+click` drills down one level when the auto-pick is too coarse.

**Storage:** comments are pane-local until sent. Once sent they become a
regular user message in the thread; the structured payload (selectors +
snippets + text) is stored on the message row so future "open this comment
again" affordances are possible.

## Migration

| Existing | Replacement | Action |
|---|---|---|
| `mode` cycle `chat → plan → design` (`ModeCycleButton.svelte`) | Top tab `Chat \| Design`; `plan` becomes a within-chat agent toggle. | DELETE the cycle button; rebuild as a 2-state segmented control in `ChatHeader`. |
| `DesignView.svelte` mounted inside `ChatView.svelte` as a slot | Real split layout owned by the chat pane shell. | DELETE `DesignView.svelte`; `ChatView` decides the split based on `pane.thread.mode`. |
| `DesignPreviewPanel.svelte` toolbar (Design pill + viewport letters M/T/D + Export button) | Toolbar redesign per § Design Pane Layout. | MIGRATE — keep the file, rewrite the toolbar. |
| `app_claude_design.go` watcher + Codex MCP server | Single `internal/design/reactor.go` dispatcher. | MIGRATE — collapse both adapters into the reactor. |
| `runClaudeDesignOptions` choice writeback | Tool-result message via the provider's tool-result protocol. | DELETE the `sendMessage` synth path; reroute through tool result. |
| `pane.pendingDesignOptions` single slot | `Map<requestId, PendingChoice>`. | MIGRATE the field; update all readers. |
| 30-min timeout in `app_claude_design.go` | None. | DELETE. |
| Existing `design_artifacts` rows | Same schema; add a sibling PNG file per row. | KEEP rows; backfill PNGs lazily on first preview, eagerly on new renders. |
| `ThreadRowBadges` "Dsn" pill | Small monochrome `frame` icon. | MIGRATE — same component, swap the rendering. |

Existing design threads survive the migration. Mode stays `design`; the new UI
just renders them in the new layout.

## Non-Goals

- **Multi-artifact canvas view** (Figma-frame style spatial layout).
- **Pop-out preview window** (preview as its own OS window).
- **Live preview of the project's actual dev server** — design mode is
  sandbox-only by definition.
- **Cross-thread comment continuity** — comments live on the thread that
  produced them.
- **Promoting a design artifact into project files** — handoff is via "Send
  to chat", which is a one-way export. No "save this artifact as
  `Component.svelte`" flow.

## Testing Strategy

Unit tests cover:

- Reactor dispatch — same input flows through Claude and Codex paths and
  produces the same artifact row.
- `present_options` concurrency — two simultaneous calls each get their own
  pending slot keyed by request id; choosing one doesn't resolve the other.
- Choice writeback — agent receives a tool-result message, transcript shows
  no synthetic user message.

Component tests cover:

- Mode tab switches active thread to most-recent-of-type or empty state.
- Design thread renders preview pane; chat thread does not.
- Composer in design mode hides chat/plan toggle, shows the design lock pill.
- Sidebar icon renders only on design threads.

Comment-mode tests are deferred with the feature.

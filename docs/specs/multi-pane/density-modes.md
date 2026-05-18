# Density Modes

> User setting controlling `min-pane-width`. Three presets.

## Decision

Three modes, exposed in Settings as a radio / dropdown:

| Mode | min-pane-width | RHS behavior | Best for |
|---|---|---|---|
| Compact | 560px | Overlays when pane < 880px | Laptops, max density, "I want to see as many panes as possible" |
| Comfortable | 880px | Always side panel; chat may sit at ~500px when RHS open | 1440p and above |
| Spacious | 1400px | Side panel; chat at its `max-w-62rem` (992px) even with RHS open | Ultrawide |

The setting is purely a `min-pane-width` constant. All downstream behavior — overflow scroll, RHS overlay-vs-sidepanel decision (see [rhs-panels.md](./rhs-panels.md)), drag-drop preview widths (see [drag-and-drop.md](./drag-and-drop.md)) — already keys off the same threshold. So the mode is one config switch.

## Default

**Compact.** Reasoning: a 1366×768 laptop in Comfortable mode (`min-pane-width = 880`) fits at most one pane before overflow scroll engages, defeating the multi-pane benefit on the user's first session. Safer to default to Compact; users with screen real estate opt into Comfortable or Spacious explicitly.

## Pane-fit at common screen sizes

Subtract ~240px for the sidebar to estimate pane-row width. Multi-pane density depends on `min-pane-width`:

| Screen | Pane row ≈ | Compact (560) | Comfortable (880) | Spacious (1400) |
|---|---|---|---|---|
| Laptop 1366 | 1126 | 2 panes | 1 pane | 1 pane |
| 1080p 1920 | 1680 | 3 panes | 1 pane | 1 pane |
| 1440p 2560 | 2320 | 4 panes | 2 panes | 1 pane |
| 4K 3840 | 3600 | 6 panes | 4 panes | 2 panes |
| Ultrawide 3440 | 3200 | 5 panes | 3 panes | 2 panes |

Going past those counts engages horizontal scroll in the pane row.

## Storage

`localStorage` under `agentOverflowPaneDensity` (separate from the layout key). Per-client. Users may want different modes per machine — main setup vs laptop.

```json
"compact" | "comfortable" | "spacious"
```

## Mode change behavior

The existing live-resize logic handles it. Switching the density mode with panes already open:

- `min-pane-width` updates → widths recompute against the new constant.
- If `min × paneCount` now exceeds pane-row width → overflow scroll engages immediately.
- If pane width crosses the 880 threshold (Compact mode only) → RHS panel morphs between overlay and side-panel.

No explicit migration step needed on mode change; everything reactively reflows.

## UI affordance

Settings panel section: "Pane density" with three radio options. Each option label includes a one-line description so users understand the trade-off without reading docs.

Optional polish (not v1-required): below each option, show "Your current screen fits ~N panes at this density" computed live.

## Implementation notes

- New store `frontend/src/lib/stores/paneDensity.svelte.ts`:
  - Reads from `localStorage` on init, writes on change.
  - Exposes `currentMode` and `minPaneWidth` (derived).
- `paneLayout.svelte.ts` and `RhsSidebarShell.svelte` consume `minPaneWidth` from this store instead of the hardcoded `DEFAULT_THREAD_PANE_MIN_WIDTH = 560`.
- Settings UI: add a new section in whatever settings component holds layout-related preferences.
- The RHS overlay threshold stays at 880px regardless of density mode — the modes just guarantee pane width is at or above 880 in Comfortable / Spacious, making overlay unreachable in those modes by definition.

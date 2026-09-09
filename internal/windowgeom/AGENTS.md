# Window geometry

This GUI-free package owns persisted normal bounds, screen-aware placement, and
debounced tracking. Wails integration belongs in internal/uiwindow and callers
own persistence.

Clamp saved bounds to the saved display when available, otherwise to the screen
with greatest overlap. Shift usable windows fully onscreen and reject placement
with no meaningful screen intersection so the caller can center it.

Never replace normal bounds with minimized, maximized, or fullscreen bounds.
Tracker retains the last normal rectangle, records display and state
separately, coalesces event bursts, and flushes synchronously on close. Keep the
package free of Wails and filesystem dependencies.

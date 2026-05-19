// Frontend custom DOM event names. Hoisted here so a rename is a
// single edit and a typo at any consumer site is a type/lookup miss at
// the constant rather than silent dead code. These travel over
// `window.dispatchEvent` / `addEventListener` — distinct from the Wails
// backend `provider:*` channel that lives in `events.ts`.
//
// This file has no imports on purpose. `events.ts` imports
// `panes.svelte.ts`, and panes (plus other stores that events depends
// on transitively) need these names — keeping the constants in a
// dependency-free module breaks the import cycle.

// Design-mode preview/handler events.
export const DESIGN_RELOAD_MAIN_EVENT = 'ao-design:reload-main';
export const DESIGN_OPTIONS_UPDATE_EVENT = 'ao-design:options-update';

// Cross-component messaging (sidebar → drawer → composer, picker chord
// → picker components, settings launcher, etc.).
export const FOCUS_TERMINAL_EVENT = 'agent-overflow:focus-terminal';
export const PICKER_TOGGLE_INPUT_EVENT = 'agent-overflow:picker-toggle-input';
export const RENAME_THREAD_EVENT = 'agent-overflow:rename-thread';
export const OPEN_SHIP_CHANGES_EVENT = 'agent-overflow:open-ship-changes';
export const OPEN_SETTINGS_EVENT = 'agent-overflow:open-settings';
export const REVEAL_PANE_EVENT = 'agent-overflow:reveal-pane';

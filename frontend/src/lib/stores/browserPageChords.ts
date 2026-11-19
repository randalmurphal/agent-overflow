import type { BrowserCompanionEvent } from './bindings';
import { browserPaneMountId, observeBrowserCompanion, sourcePaneIdForThread } from './browserCompanion.svelte';
import { companionForSource } from './companionPanes.svelte';
import { focusPane, paneTakesCommandsWhenFocused } from './panes.svelte';

// Chords pressed in a browser page: the engine claims a bound chord, the
// backend routes it to the pane mount that showed the page as a Kind
// "accelerator" event, and this replays it through the window's one
// keybinding dispatcher.

// True while a chord the page claimed is dispatched (replayBrowserAccelerator).
let replayingPageChord = false;

/**
 * Whether the keydown being dispatched is a chord pressed in a browser page.
 * It was typed with keyboard focus in the page, so it resolves outside any
 * terminal whatever focus the SPA has by the time it arrives: the engine
 * hands focus back to the SPA when it claims the chord, and the terminal
 * caret left there may already have it again.
 */
export function isReplayingBrowserPageChord(): boolean {
  return replayingPageChord;
}

/**
 * A bound chord pressed while the thread's NATIVE page view held keyboard
 * focus (spec §7). The engine swallowed it and the backend routed it here;
 * replaying it as a window keydown puts it through the one dispatcher every
 * other keystroke uses, so `when` gates, user rebinds, and the palette all
 * behave exactly as if the SPA had been focused. The companion pane is
 * focused first: the click that gave the page focus moved no DOM focus, and
 * pane-scoped commands (pane.close → this pane's tab) need the logical focus
 * to be where the user is looking. The engine already handed keyboard focus
 * to the SPA. DOM focus left in the SPA stays where it is, so a surface the
 * chord opens returns focus there on close; it gates nothing for this chord
 * (isReplayingBrowserPageChord).
 *
 * Every host client receives the event; only the one holding the pane
 * mount that showed the page (`event.mount`) replays it, and only in that
 * mount's pane: when the pane is no longer one the keyboard's commands
 * would reach (a pane promoted over it, a compact screen that no longer
 * shows it, both possible between the press and its arrival), the chord
 * is dropped rather than resolved against the pane that took its place.
 */
function replayBrowserAccelerator(event: BrowserCompanionEvent): void {
  const chord = event.accelerator;
  if (!chord || !event.mount || event.mount !== browserPaneMountId(event.threadId)) return;
  const sourcePaneId = sourcePaneIdForThread(event.threadId);
  const companion = sourcePaneId ? companionForSource(sourcePaneId, 'browser') : null;
  if (!companion || !paneTakesCommandsWhenFocused(companion.paneId)) return;
  focusPane(companion.paneId);
  replayingPageChord = true;
  try {
    window.dispatchEvent(
      new KeyboardEvent('keydown', {
        key: chord.key,
        ctrlKey: chord.ctrl === true,
        metaKey: chord.meta === true,
        altKey: chord.alt === true,
        shiftKey: chord.shift === true,
        cancelable: true,
      }),
    );
  } finally {
    replayingPageChord = false;
  }
}

observeBrowserCompanion({ accelerator: replayBrowserAccelerator });

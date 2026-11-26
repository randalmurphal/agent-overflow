// The focus a person's open asks for: a ticket a person opens, or steps
// back to, takes the focus, so a key or a paste of files goes to it (the
// card that opened it may be under it, inert, and Back is gone with the
// ticket it was on).
//
// Must keep true:
// - A request belongs to one open, named by the object that open made, and
//   only the surface showing that open takes it: a close, another open or a
//   step back ends it, and a surface mounting again because its pane moved,
//   or opened by no person's act (a restore, a thread's link), takes none.
// - A focus or a press anywhere after the open ends it, and so does a move
//   to another pane that leaves the page's focus where it was (a keyboard
//   pane move), so a surface that mounts late never takes the focus from
//   where the person put it meanwhile.
// - Taking it ends it: it is taken once.

import { untrack } from 'svelte';
import { getFocusedPaneId } from '../stores/panes.svelte';

/** One surface's request, for the opens it shows. */
export interface OpenFocusRequest<T extends object> {
  /** A person's open of `open` asks for the focus; any earlier ask ends. */
  ask(open: T): void;
  /** Reactive: whether `open` asked for the focus and nothing ended it. */
  askedFor(open: T | null): boolean;
  /** Ends the ask: taken, or the surface is gone. */
  end(): void;
}

export function createOpenFocusRequest<T extends object>(): OpenFocusRequest<T> {
  let asked = $state.raw<T | null>(null);
  let stopListening: (() => void) | null = null;
  function end(): void {
    stopListening?.();
    stopListening = null;
    asked = null;
  }
  return {
    ask(open) {
      end();
      const options = { capture: true } as const;
      document.addEventListener('focusin', end, options);
      document.addEventListener('pointerdown', end, options);
      const pane = getFocusedPaneId();
      const stopWatching = $effect.root(() => {
        $effect(() => { if (getFocusedPaneId() !== pane) untrack(end); });
      });
      stopListening = () => {
        document.removeEventListener('focusin', end, options);
        document.removeEventListener('pointerdown', end, options);
        stopWatching();
      };
      asked = open;
    },
    askedFor(open) {
      return open !== null && asked === open;
    },
    end,
  };
}

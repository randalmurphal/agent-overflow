// Pure helper that walks the thread-mode cycle used by Shift+Tab and the
// `ModeCycleButton`. Discussion mode is intentionally excluded — it can't
// be toggled into from this cycle because entering discussion is a
// separate flow (`StartDiscussion` on an existing thread).
//
// A single source of truth keeps the button and the `mode.cycle` command
// in lockstep: both import `cycleMode` and `MODE_CYCLE`, so a later
// addition (e.g. "review") needs only one edit here plus the icon map.

export type CycleMode = 'chat' | 'plan' | 'design';

export const MODE_CYCLE: readonly CycleMode[] = ['chat', 'plan', 'design'] as const;

/**
 * Return the next mode in the cycle. Falls back to "chat" when the input
 * isn't one of the cycled modes (e.g. a thread in "discussion" mode, or
 * a legacy row with mode="default"). Falling back rather than throwing
 * is intentional — the cycle button must stay usable on a thread that
 * started in a mode we don't cycle through.
 */
export function cycleMode(current: string | undefined | null): CycleMode {
  if (!current) return 'chat';
  const idx = MODE_CYCLE.indexOf(current as CycleMode);
  if (idx < 0) return 'chat';
  return MODE_CYCLE[(idx + 1) % MODE_CYCLE.length];
}

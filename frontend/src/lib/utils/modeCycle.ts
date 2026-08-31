// Pure helper that toggles between the two agent modes available inside a
// chat thread: chat ↔ plan. Discussion is an immutable thread type and is
// deliberately excluded from this cycle. The Shift+Tab keyboard shortcut and the
// AgentModeToggle button both call cycleMode so the two stay in lockstep.

export type CycleMode = 'chat' | 'plan';

export const MODE_CYCLE: readonly CycleMode[] = ['chat', 'plan'] as const;

/**
 * Return the next mode in the cycle. Falls back to "chat" when the input
 * isn't one of the cycled modes (e.g. a thread in "discussion"
 * mode, or a legacy row with mode="default"). Falling back rather than
 * throwing is intentional — the toggle must stay usable on a thread that
 * started in a mode we don't cycle through; the backend's UpdateThreadMode
 * additionally enforces the immutability of discussion threads, so
 * a stray cycle call can't actually corrupt the type.
 */
export function cycleMode(current: string | undefined | null): CycleMode {
  if (!current) return 'chat';
  const idx = MODE_CYCLE.indexOf(current as CycleMode);
  if (idx < 0) return 'chat';
  return MODE_CYCLE[(idx + 1) % MODE_CYCLE.length];
}

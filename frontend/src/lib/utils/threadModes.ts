import type { Thread } from '../types/models';

export type HiddenThreadMode = 'workflow' | 'workflow-studio' | 'workflow-triage' | 'scratch';

const HIDDEN_THREAD_MODES: ReadonlySet<string> = new Set<HiddenThreadMode>([
  'workflow',
  'workflow-studio',
  'workflow-triage',
  'scratch',
]);

// Mirrors internal/threadmode.hiddenModes. These modes are excluded only from
// discovery surfaces; callers may still open their threads directly.
export function isHiddenThreadMode(mode: Thread['mode'] | string | undefined): boolean {
  return typeof mode === 'string' && HIDDEN_THREAD_MODES.has(mode.trim());
}

/**
 * A scratch thread is an ephemeral fork: a `/side-chat` companion or an
 * agent's ask. It is hidden from listings, and its mode leaves scratch only
 * through the Keep promotion (PromoteScratchThread); the chat/plan toggle
 * cannot move it.
 */
export function isScratchThreadMode(mode: Thread['mode'] | string | undefined): boolean {
  return typeof mode === 'string' && mode.trim() === 'scratch';
}

/**
 * The mode a surface renders as. Scratch is a chat conversation with a
 * hidden row, so every layout and composer choice keyed on mode reads it as
 * chat. The row's own mode stays scratch.
 */
export function renderedThreadMode(mode: Thread['mode'] | string | undefined): string {
  const value = typeof mode === 'string' ? mode.trim() : '';
  return isScratchThreadMode(value) ? 'chat' : value;
}

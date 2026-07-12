import type { Thread } from '../types/models';

export type HiddenThreadMode = 'workflow' | 'workflow-studio' | 'workflow-triage';

const HIDDEN_THREAD_MODES: ReadonlySet<string> = new Set<HiddenThreadMode>([
  'workflow',
  'workflow-studio',
  'workflow-triage',
]);

// Mirrors internal/threadmode.hiddenModes. These modes are excluded only from
// discovery surfaces; callers may still open their threads directly.
export function isHiddenThreadMode(mode: Thread['mode'] | string | undefined): boolean {
  return typeof mode === 'string' && HIDDEN_THREAD_MODES.has(mode.trim());
}

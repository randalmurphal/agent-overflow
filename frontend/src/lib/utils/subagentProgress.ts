// Resolve what an agent card shows for a launch's counters: the live
// tick while the agent runs (stores/subagentProgress.svelte.ts, fed by
// `provider:subagent_progress`), the numbers triage persisted onto the
// launch row at its terminal (`meta.subagentProgress`) once it settled.
// Pure: the card hands in the item and whatever live tick the store has.

import type { Item } from '../types/models';
import type { SubagentProgress } from '../types/events';
import { parseJsonObject } from './parseJsonObject';

export interface ResolvedSubagentProgress {
  /** Tool calls the agent made; null when no source reported one. */
  toolUses: number | null;
  /** The agent's own token spend; null when no source reported one. */
  totalTokens: number | null;
  /** Wall-clock run time reported by the provider; null when unknown. */
  durationMs: number | null;
  /** Current activity line — only meaningful while the agent runs. */
  activity: string;
  source: 'live' | 'persisted' | 'none';
}

const NONE: ResolvedSubagentProgress = {
  toolUses: null,
  totalTokens: null,
  durationMs: null,
  activity: '',
  source: 'none',
};

function positiveInt(value: unknown): number | null {
  return typeof value === 'number' && Number.isFinite(value) && value > 0
    ? Math.floor(value)
    : null;
}

function persistedProgressOn(item: Item): SubagentProgress | null {
  const meta = parseJsonObject(item.meta);
  const raw = meta?.subagentProgress;
  if (!raw || typeof raw !== 'object' || Array.isArray(raw)) return null;
  return raw as SubagentProgress;
}

/**
 * The final numbers triage persisted for a launch, if any, read off the
 * record that settled it (docs/specs/agent-visibility.md §Immutable agent
 * history): a DETACHED launch's row is the immutable spawn event and its
 * completion record carries the numbers, so once `completion` exists the
 * launch row is not consulted; an AWAITED launch settles in place and its
 * own row carries them.
 */
export function persistedSubagentProgress(
  launch: Item,
  completion?: Item | null,
): SubagentProgress | null {
  return persistedProgressOn(completion ?? launch);
}

function fromTick(tick: SubagentProgress, source: 'live' | 'persisted'): ResolvedSubagentProgress {
  return {
    toolUses: positiveInt(tick.toolUses),
    totalTokens: positiveInt(tick.totalTokens),
    durationMs: positiveInt(tick.durationMs),
    activity: source === 'live' && typeof tick.activity === 'string' ? tick.activity.trim() : '',
    source,
  };
}

/**
 * Live wins while the row is active (its counters are newer than any
 * persisted snapshot and carry the activity line); the persisted final
 * numbers win once the row settled — a stale live tick that outlived the
 * terminal must not keep showing a mid-run count. Each falls back to the
 * other when only one exists.
 *
 * `completion` is the launch's background completion sibling once it has
 * loaded (null/undefined for an awaited launch, which settles in place).
 * It supplies liveness by default and is one of the two records the
 * persisted numbers may sit on (see persistedSubagentProgress).
 *
 * `activeOverride` lets the caller supply a better liveness answer than
 * the rows' own status: a BACKGROUND launch row stays `running` forever
 * by design (the tray invariant — the outcome lands on the completion
 * sibling), so a caller that knows more than these two rows (the tray's
 * task status) passes it here. Without it a finished background agent
 * would keep showing the last live tick's activity line.
 */
export function resolveSubagentProgress(
  launch: Item,
  completion: Item | null | undefined,
  live: SubagentProgress | undefined,
  activeOverride?: boolean,
): ResolvedSubagentProgress {
  const statusItem = completion ?? launch;
  const active =
    activeOverride ?? (statusItem.status === 'running' || statusItem.status === 'streaming');
  const persisted = persistedSubagentProgress(launch, completion);
  if (active) {
    if (live) return fromTick(live, 'live');
    return persisted ? fromTick(persisted, 'persisted') : NONE;
  }
  if (persisted) return fromTick(persisted, 'persisted');
  return live ? { ...fromTick(live, 'live'), activity: '' } : NONE;
}

/** "3 tools" / "1 tool"; empty when unknown. */
export function formatToolUses(count: number | null): string {
  if (count === null) return '';
  return `${count} ${count === 1 ? 'tool' : 'tools'}`;
}

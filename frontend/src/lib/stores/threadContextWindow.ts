import { threadMachine } from './attachedBackends.svelte';
import { getProviderDefinition } from '../providers/catalog';
import type { ContextWindow } from '../types/events';
import type { Thread } from '../types/models';
import { getSettings } from './settings.svelte';

interface PersistedTokenUsage {
  usedTokens?: number;
  maxTokens?: number;
  contextPercent?: number;
  autoCompactPercent?: number;
  autoCompactTokenLimit?: number;
  exceeded?: boolean;
}

// ---- Live per-thread usage snapshot side cache -----------------------
//
// Mid-turn `provider:usage` events arrive backend-throttled at ~2Hz per
// streaming thread. They used to rewrite Thread.lastTokenUsage through
// patchThreadEverywhere on every event — a whole-array sidebar identity
// change plus a pane.thread replacement (and a JSON re-parse of the
// string just stringified) per tick, for a field whose ONLY reader is
// seedContextWindow at pane-thread (re)seed time. The live meter on a
// visible pane never came from the row: it is written directly via
// pane.setContextWindow.
//
// So the per-event write lands here instead, and seedContextWindow
// consults this cache BEFORE Thread.lastTokenUsage. An entry, while it
// exists, is always at least as fresh as the row copy:
//
//  - recorded on every usage event (applyUsageEvent);
//  - flushed into the row and DROPPED at the turn-completion boundary
//    (applyTurnCompleted), where persisted/sidebar state converges;
//  - dropped by the provider:usage transport-gap handler BEFORE it
//    re-seeds from the re-fetched DB row (eventsTransportGap.ts) — gap
//    recovery re-pulls GetThread precisely so the meter rebuilds from
//    the persisted snapshot, and a pre-gap cache entry must never
//    shadow that;
//  - dropped on thread removal (threads.svelte.ts removeThread).
//
// Plain Map, deliberately non-reactive: every consult happens inside
// imperative (re)seed paths, and per-event meter cadence on visible
// panes still flows through pane.setContextWindow.
const liveUsageSnapshotByThread = new Map<string, string>();

/** Record the latest wire usage snapshot (JSON, the exact string that
 * previously went to Thread.lastTokenUsage) for a thread. */
export function recordLiveUsageSnapshot(threadId: string, raw: string): void {
  if (!threadId) return;
  liveUsageSnapshotByThread.set(threadId, raw);
}

/** Drop a thread's live snapshot so Thread.lastTokenUsage (the
 * authoritative row value) wins the next seed. */
export function clearLiveUsageSnapshot(threadId: string): void {
  liveUsageSnapshotByThread.delete(threadId);
}

/** Read-and-drop, for the turn-completion flush into the thread row. */
export function takeLiveUsageSnapshot(threadId: string): string | undefined {
  const raw = liveUsageSnapshotByThread.get(threadId);
  if (raw !== undefined) liveUsageSnapshotByThread.delete(threadId);
  return raw;
}

export function resetLiveUsageSnapshotsForTest(): void {
  liveUsageSnapshotByThread.clear();
}

export function seedContextWindow(nextThread: Thread | null): ContextWindow | null {
  const live = nextThread ? liveUsageSnapshotByThread.get(nextThread.id) : undefined;
  const raw = live !== undefined ? live.trim() : nextThread?.lastTokenUsage?.trim();
  if (!raw) {
    if (!nextThread?.contextWindow) return null;
    return normalizeContextWindowForThread({
      usedTokens: 0,
      maxTokens: nextThread.contextWindow,
      usedPercentage: 0,
    }, nextThread);
  }

  try {
    const parsed = JSON.parse(raw) as PersistedTokenUsage;
    if (typeof parsed.usedTokens !== 'number') return null;
    return normalizeContextWindowForThread({
      usedTokens: parsed.usedTokens,
      maxTokens: parsed.maxTokens,
      usedPercentage: parsed.contextPercent,
      autoCompactPercent: parsed.autoCompactPercent,
      autoCompactTokenLimit: parsed.autoCompactTokenLimit,
      exceeded: parsed.exceeded,
    }, nextThread);
  } catch {
    return null;
  }
}

export function normalizeContextWindowForThread(
  data: ContextWindow,
  nextThread: Thread | null,
): ContextWindow {
  const maxTokens = data.maxTokens || nextThread?.contextWindow || 0;
  const percent = nextThread
    ? activeAutoCompactPercent(nextThread, maxTokens)
    : (data.autoCompactPercent ?? 0);
  // The Go side computes UsedPercentage with a provider-aware formula
  // (Codex subtracts a 12000-token baseline; see
  // `internal/provider/usage.go:ComputeContextPercent`). Trust the wire
  // value and only fall back to the plain ratio when it's missing — that
  // path runs for legacy persisted blobs and any pre-Go-aware events.
  // Clamp to [0,100] and reject NaN/±Infinity at the seam: SVG arithmetic
  // downstream (dashOffset, color gates) breaks visibly on non-finite
  // input, and trusting the wire removes the implicit Math-coerced
  // safety the recompute used to provide.
  const usedPercentage = clampPercentage(
    data.usedPercentage ?? (maxTokens > 0 ? (data.usedTokens / maxTokens) * 100 : 0),
  );
  return {
    usedTokens: data.usedTokens,
    maxTokens,
    usedPercentage,
    ...(data.exceeded ? { exceeded: true } : {}),
    ...(percent > 0 ? {
      autoCompactPercent: percent,
      autoCompactTokenLimit: maxTokens > 0
        ? Math.floor(maxTokens * percent / 100)
        : data.autoCompactTokenLimit,
    } : {}),
  };
}

function clampPercentage(value: number): number {
  if (!Number.isFinite(value)) return 0;
  if (value < 0) return 0;
  if (value > 100) return 100;
  return value;
}

export function activeAutoCompactPercent(
  nextThread: Thread,
  effectiveContextWindow: number = nextThread.contextWindow ?? 0,
): number {
  // Per-thread override wins when set (chat-meter edit flow). Otherwise
  // fall back to the per-provider Settings value, then the absolute 90%
  // safety default if Settings hasn't been loaded yet.
  const isExtended = effectiveContextWindow >= 1_000_000;
  const override = isExtended
    ? nextThread.autoCompactExtendedPercent ?? 0
    : nextThread.autoCompactStandardPercent ?? 0;
  if (override > 0) return override;

  const settings = getSettings(threadMachine(nextThread.id, nextThread.projectId));
  const providerSettings = getProviderDefinition(nextThread.provider).settings;
  const providerSetting = isExtended
    ? settings[providerSettings.extendedCompactKey]
    : settings[providerSettings.standardCompactKey];
  return providerSetting > 0 ? providerSetting : 90;
}

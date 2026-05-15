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
}

export function seedContextWindow(nextThread: Thread | null): ContextWindow | null {
  const raw = nextThread?.lastTokenUsage?.trim();
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
  return {
    usedTokens: data.usedTokens,
    maxTokens,
    usedPercentage: maxTokens > 0 ? (data.usedTokens / maxTokens) * 100 : data.usedPercentage,
    ...(percent > 0 ? {
      autoCompactPercent: percent,
      autoCompactTokenLimit: maxTokens > 0
        ? Math.floor(maxTokens * percent / 100)
        : data.autoCompactTokenLimit,
    } : {}),
  };
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

  const settings = getSettings();
  const providerSettings = getProviderDefinition(nextThread.provider).settings;
  const providerSetting = isExtended
    ? settings[providerSettings.extendedCompactKey]
    : settings[providerSettings.standardCompactKey];
  return providerSetting > 0 ? providerSetting : 90;
}

import type { Item } from '../types/models';
import { parseJsonObject } from './parseJsonObject';

export interface CommandAgentResult {
  launchId: string;
  sourceKind: string;
  sourceName: string;
}

/**
 * Return the typed source of a provider command result produced by a forked
 * agent. Every field is required before presentation changes. Older or
 * malformed command-result rows therefore keep their terminal-style fallback.
 */
export function commandAgentResult(item: Item): CommandAgentResult | null {
  if (item.kind !== 'command_result') return null;
  const raw = parseJsonObject(item.meta)?.agentResult;
  if (!raw || typeof raw !== 'object' || Array.isArray(raw)) return null;
  const source = raw as Record<string, unknown>;
  if (
    typeof source.launchId !== 'string' || !source.launchId.trim()
    || typeof source.sourceKind !== 'string' || !source.sourceKind.trim()
    || typeof source.sourceName !== 'string' || !source.sourceName.trim()
  ) return null;
  return {
    launchId: source.launchId.trim(),
    sourceKind: source.sourceKind.trim(),
    sourceName: source.sourceName.trim(),
  };
}

export function isCommandAgentResult(item: Item): boolean {
  return commandAgentResult(item) !== null;
}

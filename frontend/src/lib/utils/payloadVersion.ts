import type { Item } from '../types/models';
import { parseJsonObject } from './parseJsonObject';

const PAYLOAD_VERSION_EDGE_CHARS = 64;
const PAYLOAD_VERSION_INLINE_MAX_CHARS = 160;
export const THINKING_PAYLOAD_EXPANSION_STATE_KEY = 'thinking-full';
export const COMPACTION_PAYLOAD_EXPANSION_STATE_KEY = 'compaction-full';
// The live "compact" reasoning tail (its own row, above the compaction
// divider). It rides the same streaming-payload-expansion machinery as
// thinking — `thinkingPayloadVersionForItem` / `thinkingPayloadCacheEnabled`
// are item-keyed and kind-agnostic — but under its own namespace so a
// reasoning row and a divider for the same compaction never share an entry.
export const COMPACTION_REASONING_PAYLOAD_EXPANSION_STATE_KEY =
  'compaction-reasoning-full';
// Output of a provider-executed slash command whose text exceeded the inline
// bound. The payload is written once with the row and never grows, so the
// default module cache and `payloadVersionForItem` are correct here.
export const COMMAND_RESULT_PAYLOAD_EXPANSION_STATE_KEY = 'command-result-full';

function stableStringHash(value: string): string {
  let hash = 0x811c9dc5;
  for (let index = 0; index < value.length; index += 1) {
    hash ^= value.charCodeAt(index);
    hash = Math.imul(hash, 0x01000193);
  }
  return (hash >>> 0).toString(16).padStart(8, '0');
}

export function boundedPayloadVersionString(value: string): string {
  if (value.length <= PAYLOAD_VERSION_INLINE_MAX_CHARS) return value;
  const head = value.slice(0, PAYLOAD_VERSION_EDGE_CHARS);
  const tail = value.slice(-PAYLOAD_VERSION_EDGE_CHARS);
  return `${value.length}:${stableStringHash(value)}:${head}:${tail}`;
}

function readPayloadSignature(meta: string | undefined): string | undefined {
  if (!meta) return undefined;
  const record = parseJsonObject(meta);
  if (!record) return undefined;
  const signature =
    record.signature ??
    record.payloadSignature ??
    record.payload_signature ??
    record.sha256 ??
    record.hash;
  return typeof signature === 'string' && signature.trim() !== ''
    ? signature
    : undefined;
}

export function payloadVersionForItem(item: Item | undefined): unknown {
  if (!item) return undefined;
  const payloadMeta =
    item.payloadMeta && item.payloadMeta.trim() !== ''
      ? item.payloadMeta
      : undefined;
  return (
    readPayloadSignature(item.payloadMeta) ??
    readPayloadSignature(item.meta) ??
    item.payloadId ??
    item.inputPayloadId ??
    (payloadMeta ? boundedPayloadVersionString(payloadMeta) : undefined) ??
    item.updatedAt
  );
}

// threadRowUiState registry entries outlive the row component that creates
// them and retain option callbacks for the entry's whole lifetime — helpers
// passed as `cacheEnabled`/`payloadVersion` must live at module scope and
// capture nothing from a component instance, or the entry pins that
// instance's context (and its detached DOM) until the item is pruned.
export function thinkingPayloadCacheEnabled(item: Item | undefined): boolean {
  return item?.status !== 'streaming';
}

export function thinkingPayloadVersionForItem(item: Item | undefined): unknown {
  if (!item) return undefined;
  if (item.status === 'streaming') {
    return JSON.stringify([item.payloadId ?? '', 'streaming']);
  }
  return JSON.stringify([item.payloadId ?? '', item.status, item.updatedAt]);
}

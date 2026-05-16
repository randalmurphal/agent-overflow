import type { Item } from '../types/models';
import { parseJsonObject } from './parseJsonObject';

const PAYLOAD_VERSION_EDGE_CHARS = 64;
const PAYLOAD_VERSION_INLINE_MAX_CHARS = 160;

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
  const payloadMeta = item.payloadMeta && item.payloadMeta.trim() !== ''
    ? item.payloadMeta
    : undefined;
  return (
    readPayloadSignature(item.payloadMeta)
    ?? readPayloadSignature(item.meta)
    ?? item.payloadId
    ?? item.inputPayloadId
    ?? (payloadMeta ? boundedPayloadVersionString(payloadMeta) : undefined)
    ?? item.updatedAt
  );
}

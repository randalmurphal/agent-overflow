import { fnv1a32 } from './fnv1a';

/** The review comment source key of a patch's text, persisted with each
 * draft. */
export function diffSourceKey(text: string): string {
  return formatSourceKey(fnv1a32(text), text.length);
}

/** The source key (`diffSourceKey`) of text of `units` UTF-16 code units
 * whose fnv1a32 is `hash`: `fnv1a:<hash as 8 hex>:<units>`. */
export function formatSourceKey(hash: number, units: number): string {
  return `fnv1a:${hash.toString(16).padStart(8, '0')}:${units}`;
}

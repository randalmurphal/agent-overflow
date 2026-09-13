// Per-block line-wrap choice for fenced code. Blocks wrap by default; the
// host's overlay toggle unwraps one block so long lines keep their layout
// and the pre pans horizontally (app.css pan-x rules).
//
// Keyed by highlight identity (fence language word + source content key),
// never by DOM position: a settled block is retired into static HTML, and
// the virtualizer unmounts rows that scroll away, so the choice has to
// outlive both. Content keys retain no source text. Two blocks with the
// same source share the choice, which is the desired reading of "this
// code". Bounded LRU by Map insertion order.

import { contentKey } from '../../../utils/fnv1a';

export const CODE_WRAP_STATE_MAX_ENTRIES = 256;

const unwrapped = new Map<string, true>();

function key(lang: string, sourceKey: string): string {
  return `${lang} ${sourceKey}`;
}

/** `sourceKey` is a `contentKey` of the fence source. */
export function isCodeBlockUnwrappedByKey(lang: string, sourceKey: string): boolean {
  return unwrapped.has(key(lang, sourceKey));
}

export function isCodeBlockUnwrapped(lang: string, source: string): boolean {
  return isCodeBlockUnwrappedByKey(lang, contentKey(source));
}

export function setCodeBlockUnwrappedByKey(lang: string, sourceKey: string, next: boolean): void {
  const k = key(lang, sourceKey);
  unwrapped.delete(k);
  if (!next) return;
  unwrapped.set(k, true);
  while (unwrapped.size > CODE_WRAP_STATE_MAX_ENTRIES) {
    const oldest = unwrapped.keys().next().value;
    if (oldest === undefined) break;
    unwrapped.delete(oldest);
  }
}

export function setCodeBlockUnwrapped(lang: string, source: string, next: boolean): void {
  setCodeBlockUnwrappedByKey(lang, contentKey(source), next);
}

export function resetCodeWrapStateForTest(): void {
  unwrapped.clear();
}

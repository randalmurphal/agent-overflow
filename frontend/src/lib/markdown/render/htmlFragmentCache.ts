// Bounded per-Document LRU of parsed static-markdown DocumentFragments.
// Virtualized scroll-back and thread switches remount the same settled
// markdown blocks repeatedly; cloning a detached fragment avoids running
// Blink's HTML parser (`template.innerHTML`) from scratch on every remount
// while keeping retained memory strictly capped.

const MAX_FRAGMENT_ENTRIES = 512;
const MAX_FRAGMENT_CHARS = 512_000;
export const MAX_SINGLE_FRAGMENT_CHARS = 32_768;

interface FragmentCacheEntry {
  readonly fragment: DocumentFragment;
  readonly chars: number;
}

export interface FragmentCache {
  readonly map: Map<string, FragmentCacheEntry>;
  totalChars: number;
}

const templatesByDocument = new WeakMap<Document, HTMLTemplateElement>();
const fragmentCachesByDocument = new WeakMap<Document, FragmentCache>();

export function reusableTemplate(ownerDocument: Document): HTMLTemplateElement {
  let template = templatesByDocument.get(ownerDocument);
  if (!template) {
    template = ownerDocument.createElement('template');
    templatesByDocument.set(ownerDocument, template);
  }
  return template;
}

export function getFragmentCache(ownerDocument: Document): FragmentCache | undefined {
  return fragmentCachesByDocument.get(ownerDocument);
}

function ensureFragmentCache(ownerDocument: Document): FragmentCache {
  let cache = fragmentCachesByDocument.get(ownerDocument);
  if (!cache) {
    cache = { map: new Map(), totalChars: 0 };
    fragmentCachesByDocument.set(ownerDocument, cache);
  }
  return cache;
}

export function getCachedFragmentClone(
  ownerDocument: Document,
  html: string,
): DocumentFragment | null {
  if (html.length > MAX_SINGLE_FRAGMENT_CHARS) return null;
  const cache = fragmentCachesByDocument.get(ownerDocument);
  if (!cache) return null;
  const entry = cache.map.get(html);
  if (!entry) return null;
  cache.map.delete(html);
  cache.map.set(html, entry);
  return entry.fragment.cloneNode(true) as DocumentFragment;
}

export function storeCachedFragment(
  ownerDocument: Document,
  html: string,
  fragment: DocumentFragment,
): void {
  if (html.length === 0 || html.length > MAX_SINGLE_FRAGMENT_CHARS) return;
  const cache = ensureFragmentCache(ownerDocument);
  const existing = cache.map.get(html);
  if (existing) {
    cache.totalChars -= existing.chars;
    cache.map.delete(html);
  }
  cache.map.set(html, { fragment, chars: html.length });
  cache.totalChars += html.length;
  while (cache.map.size > MAX_FRAGMENT_ENTRIES || cache.totalChars > MAX_FRAGMENT_CHARS) {
    const oldestKey = cache.map.keys().next().value;
    if (oldestKey === undefined) break;
    const oldest = cache.map.get(oldestKey);
    cache.map.delete(oldestKey);
    if (oldest) cache.totalChars -= oldest.chars;
  }
}

export function acquireCachedHtmlFragment(
  ownerDocument: Document,
  html: string,
): DocumentFragment {
  const cached = getCachedFragmentClone(ownerDocument, html);
  if (cached) return cached;
  const template = reusableTemplate(ownerDocument);
  template.innerHTML = html;
  if (html.length > 0 && html.length <= MAX_SINGLE_FRAGMENT_CHARS) {
    storeCachedFragment(
      ownerDocument,
      html,
      template.content.cloneNode(true) as DocumentFragment,
    );
  }
  return template.content;
}

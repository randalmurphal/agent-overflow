import type { Item } from '../types/models';
import type { AttachmentPreviewCache, ImagePreviewItem } from '../utils/attachmentPreview.svelte';
import { userMessageIdentity } from '../utils/userMessageIdentity';
import { createKeyedSignalRegistry } from './keyedSignalRegistry.svelte';

interface MessageAttachments {
  previews: Map<string, ImagePreviewItem>;
  urls: Set<string>;
}

interface Options {
  getItemById(itemId: string): Item | undefined;
  loadedItems(): Iterable<Item>;
}

/** Message presentation survives confirmation, and ends when the message leaves retention. */
export function createThreadMessageUiState(options: Options) {
  const expandedKeys = new Set<string>();
  const expandedSignals = createKeyedSignalRegistry<boolean>(false);
  const attachments = new Map<string, MessageAttachments>();

  function itemKey(item: Item): string {
    return userMessageIdentity(item) ?? `item:${item.id}`;
  }

  function keyForId(itemId: string): string {
    const item = options.getItemById(itemId);
    return item ? itemKey(item) : `item:${itemId}`;
  }

  // An entry is a message key, or a message key and the part of the message
  // that is open (a shell output block), so a part's state is retained and
  // released with its message.
  const PART = '\u0000';

  function messageKeyOf(entry: string): string {
    const at = entry.indexOf(PART);
    return at < 0 ? entry : entry.slice(0, at);
  }

  function entryFor(itemId: string, part: string): string {
    const key = keyForId(itemId);
    return part ? key + PART + part : key;
  }

  function isExpanded(itemId: string, part = ''): boolean {
    return expandedSignals.get(entryFor(itemId, part));
  }

  function setExpanded(itemId: string, value: boolean, part = ''): void {
    const key = entryFor(itemId, part);
    if (expandedKeys.has(key) === value) return;
    if (value) expandedKeys.add(key);
    else expandedKeys.delete(key);
    expandedSignals.set(key, value);
  }

  function revoke(preview: ImagePreviewItem): void {
    if (preview.url.startsWith('blob:')) URL.revokeObjectURL(preview.url);
  }

  function disposeAttachments(key: string): void {
    const cache = attachments.get(key);
    if (!cache) return;
    for (const url of cache.urls) URL.revokeObjectURL(url);
    cache.previews.clear();
    cache.urls.clear();
    attachments.delete(key);
  }

  function attachmentCacheFor(itemId: string): AttachmentPreviewCache {
    const key = keyForId(itemId);
    let cache = attachments.get(key);
    if (!cache) {
      cache = { previews: new Map(), urls: new Set() };
      attachments.set(key, cache);
    }
    const owned = cache;
    return {
      get: (id) => attachments.get(key) === owned ? owned.previews.get(id) : undefined,
      set(id, preview) {
        if (attachments.get(key) !== owned) {
          revoke(preview);
          return;
        }
        // Mounted readers can still hold an earlier preview of this attachment.
        // Release every cache-owned URL when the message leaves retention.
        if (preview.url.startsWith('blob:')) owned.urls.add(preview.url);
        owned.previews.set(id, preview);
      },
    };
  }

  function disposeItems(items: readonly Item[]): void {
    let retainedKeys: Set<string> | undefined;
    const open = expandedKeys.size > 0 ? new Set(Array.from(expandedKeys, messageKeyOf)) : null;
    const released = new Set<string>();
    for (const item of items) {
      const key = itemKey(item);
      if (!(open?.has(key)) && !attachments.has(key)) continue;
      if (userMessageIdentity(item) !== null) {
        retainedKeys ??= new Set(Array.from(options.loadedItems(), itemKey));
        if (retainedKeys.has(key)) continue;
      }
      disposeAttachments(key);
      released.add(key);
    }
    if (released.size === 0 || expandedKeys.size === 0) return;
    for (const entry of Array.from(expandedKeys)) {
      if (released.has(messageKeyOf(entry))) {
        expandedKeys.delete(entry);
        expandedSignals.drop(entry);
      }
    }
  }

  function prune(itemIds: ReadonlySet<string>): void {
    if (expandedKeys.size === 0 && attachments.size === 0) return;
    const kept = new Set(Array.from(itemIds, keyForId));
    for (const key of attachments.keys()) {
      if (!kept.has(key)) disposeAttachments(key);
    }
    for (const entry of Array.from(expandedKeys)) {
      if (!kept.has(messageKeyOf(entry))) {
        expandedKeys.delete(entry);
        expandedSignals.drop(entry);
      }
    }
  }

  function clear(): void {
    expandedKeys.clear();
    expandedSignals.reset();
    for (const key of attachments.keys()) disposeAttachments(key);
  }

  return {
    isExpanded, setExpanded, attachmentCacheFor, disposeItems, prune, clear,
    expansionSignature: () => expandedKeys.size > 0 ? `u:${JSON.stringify([...expandedKeys].sort())}` : '',
    stats: () => ({ expandedUserMessages: expandedKeys.size, attachmentItems: attachments.size }),
  };
}

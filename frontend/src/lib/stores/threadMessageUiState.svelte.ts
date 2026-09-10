import type { Item } from '../types/models';
import type { AttachmentPreviewCache, ImagePreviewItem } from '../utils/attachmentPreview.svelte';
import { userMessageIdentity } from '../utils/userMessageIdentity';

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
  let expanded = $state<ReadonlySet<string>>(new Set());
  const attachments = new Map<string, MessageAttachments>();

  function itemKey(item: Item): string {
    return userMessageIdentity(item) ?? `item:${item.id}`;
  }

  function keyForId(itemId: string): string {
    const item = options.getItemById(itemId);
    return item ? itemKey(item) : `item:${itemId}`;
  }

  function isExpanded(itemId: string): boolean {
    return expanded.has(keyForId(itemId));
  }

  function setExpanded(itemId: string, value: boolean): void {
    const key = keyForId(itemId);
    if (expanded.has(key) === value) return;
    const next = new Set(expanded);
    if (value) next.add(key);
    else next.delete(key);
    expanded = next;
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
    let next: Set<string> | undefined;
    for (const item of items) {
      const key = itemKey(item);
      if (!expanded.has(key) && !attachments.has(key)) continue;
      if (userMessageIdentity(item) !== null) {
        retainedKeys ??= new Set(Array.from(options.loadedItems(), itemKey));
        if (retainedKeys.has(key)) continue;
      }
      disposeAttachments(key);
      if (expanded.has(key)) (next ??= new Set(expanded)).delete(key);
    }
    if (next) expanded = next;
  }

  function prune(itemIds: ReadonlySet<string>): void {
    if (expanded.size === 0 && attachments.size === 0) return;
    const kept = new Set(Array.from(itemIds, keyForId));
    for (const key of attachments.keys()) {
      if (!kept.has(key)) disposeAttachments(key);
    }
    const next = new Set([...expanded].filter((key) => kept.has(key)));
    if (next.size !== expanded.size) expanded = next;
  }

  function clear(): void {
    expanded = new Set();
    for (const key of attachments.keys()) disposeAttachments(key);
  }

  return {
    isExpanded, setExpanded, attachmentCacheFor, disposeItems, prune, clear,
    expansionSignature: () => expanded.size > 0 ? `u:${JSON.stringify([...expanded].sort())}` : '',
    stats: () => ({ expandedUserMessages: expanded.size, attachmentItems: attachments.size }),
  };
}

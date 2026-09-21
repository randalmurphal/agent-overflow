import type { Item } from '../types/models';

function rowChars(item: Item): number {
  return (item.summary?.length ?? 0) + (item.meta?.length ?? 0)
    + (item.payloadMeta?.length ?? 0) + (item.payloadPreviewSpans?.length ?? 0);
}

/** Page context has one level and shares the row's retention lifetime. */
export function retainedItemChars(item: Item): number {
  return rowChars(item) + (item.completionLaunch ? rowChars(item.completionLaunch) : 0);
}

/** Flatten the single-level context too: an Item can be a Svelte proxy. */
export function snapshotItem(item: Item): Item {
  return {
    ...item,
    completionLaunch: item.completionLaunch
      ? { ...item.completionLaunch, completionLaunch: undefined }
      : undefined,
  };
}

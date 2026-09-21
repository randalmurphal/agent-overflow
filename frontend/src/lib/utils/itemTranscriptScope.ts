import type { Item } from '../types/models';

/** The transcript containing a row, including parentless completion siblings. */
export function itemTranscriptScope(item: Item, getItem: (id: string) => Item | undefined): string {
  if (item.parentId) return item.parentId;
  if (!item.completionOf) return '';
  const launch = getItem(item.completionOf) ?? item.completionLaunch;
  return launch?.id === item.completionOf && launch.threadId === item.threadId ? launch.parentId ?? '' : '';
}

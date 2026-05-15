export type PaneLayoutKind = 'thread';

export interface PaneLayoutItem {
  id: string;
  paneId: string;
  kind: PaneLayoutKind;
  minWidth: number;
}

export const DEFAULT_THREAD_PANE_MIN_WIDTH = 560;

let layoutItems: PaneLayoutItem[] = $state([
  {
    id: 'main',
    paneId: 'main',
    kind: 'thread',
    minWidth: DEFAULT_THREAD_PANE_MIN_WIDTH,
  },
]);

export function getPaneLayoutItems(): PaneLayoutItem[] {
  return layoutItems;
}

export function removePaneLayoutItem(paneId: string): void {
  const next = layoutItems.filter((item) => item.paneId !== paneId);
  if (next.length === layoutItems.length) return;
  layoutItems = next.length > 0 ? next : [
    {
      id: 'main',
      paneId: 'main',
      kind: 'thread',
      minWidth: DEFAULT_THREAD_PANE_MIN_WIDTH,
    },
  ];
}

export function resetPaneLayoutForTest(): void {
  layoutItems = [
    {
      id: 'main',
      paneId: 'main',
      kind: 'thread',
      minWidth: DEFAULT_THREAD_PANE_MIN_WIDTH,
    },
  ];
}

export function setPaneLayoutItemsForTest(items: PaneLayoutItem[]): void {
  layoutItems = items.map((item) => ({ ...item }));
}

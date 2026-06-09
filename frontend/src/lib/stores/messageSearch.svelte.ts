// Message-search dialog open/close state. Mirrors palette / cheatSheet.
//
// `mode` selects what the one overlay searches: 'global' is the cross-thread
// title+message search (mod+shift+f); 'thread' is the in-thread find scoped to
// the target pane's thread (mod+f). One component renders both so the input,
// result list, and keyboard navigation stay shared.

export type MessageSearchMode = 'global' | 'thread';

let open = $state(false);
let targetPaneId: string | null = $state(null);
let mode: MessageSearchMode = $state('global');

export function isMessageSearchOpen(): boolean {
  return open;
}

export function getMessageSearchTargetPaneId(): string | null {
  return targetPaneId;
}

export function getMessageSearchMode(): MessageSearchMode {
  return mode;
}

export function openMessageSearch(
  paneId: string | null = null,
  searchMode: MessageSearchMode = 'global',
): void {
  targetPaneId = paneId;
  mode = searchMode;
  open = true;
}

export function closeMessageSearch(): void {
  open = false;
  targetPaneId = null;
  mode = 'global';
}

export function toggleMessageSearch(
  paneId: string | null = null,
  searchMode: MessageSearchMode = 'global',
): void {
  if (open) closeMessageSearch();
  else openMessageSearch(paneId, searchMode);
}

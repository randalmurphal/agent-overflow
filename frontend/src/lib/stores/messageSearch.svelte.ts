// Message-search dialog open/close state: the cross-thread title and message
// search (mod+shift+f). Mirrors palette / cheatSheet. Finding inside one pane
// is the pane's find bar (paneFind.svelte.ts), not this dialog.

let open = $state(false);
let targetPaneId: string | null = $state(null);

export function isMessageSearchOpen(): boolean {
  return open;
}

export function getMessageSearchTargetPaneId(): string | null {
  return targetPaneId;
}

export function openMessageSearch(paneId: string | null = null): void {
  targetPaneId = paneId;
  open = true;
}

export function closeMessageSearch(): void {
  open = false;
  targetPaneId = null;
}

export function toggleMessageSearch(paneId: string | null = null): void {
  if (open) closeMessageSearch();
  else openMessageSearch(paneId);
}

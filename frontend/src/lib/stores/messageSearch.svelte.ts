// Message-search dialog open/close state. Mirrors palette / cheatSheet.

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

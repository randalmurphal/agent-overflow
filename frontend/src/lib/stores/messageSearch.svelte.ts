// Message-search dialog open/close state. Mirrors palette / cheatSheet.

let open = $state(false);

export function isMessageSearchOpen(): boolean {
  return open;
}

export function openMessageSearch(): void {
  open = true;
}

export function closeMessageSearch(): void {
  open = false;
}

export function toggleMessageSearch(): void {
  open = !open;
}

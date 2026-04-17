// Keyboard-shortcut cheat-sheet open/close state. Mirrors the palette
// store — any component can flip the sheet from anywhere.

let open = $state(false);

export function isCheatSheetOpen(): boolean {
  return open;
}

export function openCheatSheet(): void {
  open = true;
}

export function closeCheatSheet(): void {
  open = false;
}

export function toggleCheatSheet(): void {
  open = !open;
}

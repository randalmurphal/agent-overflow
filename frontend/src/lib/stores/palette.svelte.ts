// Command palette open/close state. Kept as a tiny module so any component
// can trigger the palette without prop drilling.

let open = $state(false);
let targetPaneId: string | null = $state(null);

export function isPaletteOpen(): boolean {
  return open;
}

export function getPaletteTargetPaneId(): string | null {
  return targetPaneId;
}

export function openPalette(paneId: string | null = null): void {
  targetPaneId = paneId;
  open = true;
}

export function closePalette(): void {
  open = false;
  targetPaneId = null;
}

export function togglePalette(paneId: string | null = null): void {
  if (open) closePalette();
  else openPalette(paneId);
}

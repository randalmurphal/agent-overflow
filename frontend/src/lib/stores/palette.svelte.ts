// Command palette open/close state. Kept as a tiny module so any component
// can trigger the palette without prop drilling.

let open = $state(false);

export function isPaletteOpen(): boolean {
  return open;
}

export function openPalette(): void {
  open = true;
}

export function closePalette(): void {
  open = false;
}

export function togglePalette(): void {
  open = !open;
}

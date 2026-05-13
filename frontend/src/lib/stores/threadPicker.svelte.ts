// Thread-picker dialog open/close state. Mirrors palette / cheatSheet /
// messageSearch — any component can flip the picker from anywhere.

let open = $state(false);
let targetPaneId: string | null = $state(null);

export function isThreadPickerOpen(): boolean {
  return open;
}

export function getThreadPickerTargetPaneId(): string | null {
  return targetPaneId;
}

export function openThreadPicker(paneId: string | null = null): void {
  targetPaneId = paneId;
  open = true;
}

export function closeThreadPicker(): void {
  open = false;
  targetPaneId = null;
}

export function toggleThreadPicker(paneId: string | null = null): void {
  if (open) closeThreadPicker();
  else openThreadPicker(paneId);
}

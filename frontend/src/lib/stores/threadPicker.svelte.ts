// Thread-picker dialog open/close state. Mirrors palette / cheatSheet /
// messageSearch — any component can flip the picker from anywhere.

let open = $state(false);

export function isThreadPickerOpen(): boolean {
  return open;
}

export function openThreadPicker(): void {
  open = true;
}

export function closeThreadPicker(): void {
  open = false;
}

export function toggleThreadPicker(): void {
  open = !open;
}

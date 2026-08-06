// Account-switcher picker open/close state. Mirrors palette / cheatSheet /
// threadPicker — any component can flip it from anywhere.
//
// No target pane: unlike the thread picker, switching a provider account is an
// app-wide action with nothing pane-scoped to resolve against.

let open = $state(false);

export function isAccountSwitcherOpen(): boolean {
  return open;
}

export function openAccountSwitcher(): void {
  open = true;
}

export function closeAccountSwitcher(): void {
  open = false;
}

export function toggleAccountSwitcher(): void {
  open = !open;
}

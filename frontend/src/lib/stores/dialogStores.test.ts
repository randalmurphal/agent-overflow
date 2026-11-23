// The dialog open/close stores: palette, message search and thread picker
// take a target pane, the cheat sheet and the account switcher do not.
// Each store runs one sequence from the state its module starts in, so the
// first assertion of each test is the store's initial state.

import { expect, it } from 'vitest';
import { closeAccountSwitcher, isAccountSwitcherOpen, toggleAccountSwitcher } from './accountSwitcher.svelte';
import { closeCheatSheet, isCheatSheetOpen, openCheatSheet } from './cheatSheet.svelte';
import { closeMessageSearch, getMessageSearchTargetPaneId, isMessageSearchOpen, openMessageSearch } from './messageSearch.svelte';
import { closePalette, getPaletteTargetPaneId, isPaletteOpen, openPalette } from './palette.svelte';
import { closeThreadPicker, getThreadPickerTargetPaneId, isThreadPickerOpen, openThreadPicker } from './threadPicker.svelte';

it.each([
  { name: 'palette', isOpen: isPaletteOpen, target: getPaletteTargetPaneId, open: openPalette, close: closePalette },
  { name: 'message search', isOpen: isMessageSearchOpen, target: getMessageSearchTargetPaneId, open: openMessageSearch, close: closeMessageSearch },
  { name: 'thread picker', isOpen: isThreadPickerOpen, target: getThreadPickerTargetPaneId, open: openThreadPicker, close: closeThreadPicker },
])('the $name opens on a pane, retargets, and closes', ({ isOpen, target, open, close }) => {
  const state = () => ({ open: isOpen(), pane: target() });
  expect(state()).toEqual({ open: false, pane: null });
  open('pane-a');
  expect(state()).toEqual({ open: true, pane: 'pane-a' });
  open('pane-b');
  expect(state()).toEqual({ open: true, pane: 'pane-b' });
  open('pane-b');
  expect(state()).toEqual({ open: true, pane: 'pane-b' });
  open();
  expect(state()).toEqual({ open: true, pane: null });
  open('pane-a');
  close();
  expect(state()).toEqual({ open: false, pane: null });
  close();
  expect(state()).toEqual({ open: false, pane: null });
});

it('the cheat sheet opens and closes', () => {
  expect(isCheatSheetOpen()).toBe(false);
  openCheatSheet();
  expect(isCheatSheetOpen()).toBe(true);
  openCheatSheet();
  expect(isCheatSheetOpen()).toBe(true);
  closeCheatSheet();
  expect(isCheatSheetOpen()).toBe(false);
  closeCheatSheet();
  expect(isCheatSheetOpen()).toBe(false);
});

it('the account switcher toggles and closes', () => {
  expect(isAccountSwitcherOpen()).toBe(false);
  toggleAccountSwitcher();
  expect(isAccountSwitcherOpen()).toBe(true);
  toggleAccountSwitcher();
  expect(isAccountSwitcherOpen()).toBe(false);
  toggleAccountSwitcher();
  closeAccountSwitcher();
  expect(isAccountSwitcherOpen()).toBe(false);
  closeAccountSwitcher();
  expect(isAccountSwitcherOpen()).toBe(false);
});

import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { installNativeKeybindings } from './keybindings';
import { prepareNativeShell } from './boot';
import { KEYBINDING_DEFAULTS } from '../generated/keybindingDefaults';
import { getKeybindingRules, loadKeybindings, resyncKeybindings, resetKeybindingsStore,
  resetKeybindingsToDefaults, saveKeybindings, setKeybindingPersistence, withReboundRow } from '../stores/keybindings.svelte';
import { readFrontendValue, writeFrontendValue } from '../stores/frontendStorage';
import { getToasts, removeToast } from '../stores/toast.svelte';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { resetStagedBackends } from '../../test/helpers/backends';
import { detachBackend } from '../transport/backends';

beforeEach(() => {
  localStorage.clear();
  resetKeybindingsStore();
  for (const toast of [...getToasts()]) removeToast(toast.id);
});
afterEach(() => {
  setKeybindingPersistence(null);
  resetKeybindingsStore();
  localStorage.clear();
  vi.unstubAllGlobals();
  resetStagedBackends();
});

it('native boot loads and edits shortcuts without a HOME connection or any remote RPC', async () => {
  const rpc = vi.fn(() => { throw new Error('A phone shortcut must not access a remote host'); });
  for (const method of ['GetKeybindings', 'UpdateKeybindings', 'ResetKeybindings']) setBindingMock(method, rpc);
  vi.stubGlobal('Capacitor', { isNativePlatform: () => true });
  detachBackend('');
  expect(prepareNativeShell()).toEqual({ shell: true, paired: false });
  await loadKeybindings();
  expect(getKeybindingRules()).toEqual(KEYBINDING_DEFAULTS);
  const primary = getKeybindingRules().find((rule) => rule.defaultId === 'thread.new.primary')!;
  await saveKeybindings(withReboundRow(getKeybindingRules(), primary, 'ctrl+alt+n'));
  await resyncKeybindings(); // A host reconnect/update cannot overwrite local choices.
  expect(getKeybindingRules().find((rule) => rule.defaultId === primary.defaultId)?.key).toBe('ctrl+alt+n');
  await resetKeybindingsToDefaults();
  expect(getKeybindingRules()).toEqual(KEYBINDING_DEFAULTS);
  expect(rpc).not.toHaveBeenCalled();
  expect(getToasts()).toEqual([]);
});

it('persists only changed chords by stable identity across reloads, including an unbound alternate', async () => {
  installNativeKeybindings();
  await loadKeybindings();
  const primary = getKeybindingRules().find((rule) => rule.defaultId === 'thread.new.primary')!;
  const alternate = getKeybindingRules().find((rule) => rule.defaultId === 'thread.new.alternate')!;
  await saveKeybindings(withReboundRow(withReboundRow(getKeybindingRules(), primary, 'ctrl+alt+n'), alternate, ''));
  expect(readFrontendValue('keybindings')).toEqual({ 'thread.new.primary': 'ctrl+alt+n', 'thread.new.alternate': '' });
  resetKeybindingsStore();
  installNativeKeybindings();
  await loadKeybindings();
  expect(getKeybindingRules().find((rule) => rule.defaultId === primary.defaultId)?.key).toBe('ctrl+alt+n');
  expect(getKeybindingRules().find((rule) => rule.defaultId === alternate.defaultId)?.key).toBe('');
  await saveKeybindings(withReboundRow(getKeybindingRules(), primary, primary.defaultKey!));
  expect(readFrontendValue('keybindings')).toEqual({ 'thread.new.alternate': '' });
});

it('rejects corrupt saved chords and obsolete identities without losing shipped defaults', async () => {
  installNativeKeybindings();
  writeFrontendValue('keybindings', { 'thread.new.primary': 42, 'thread.new.alternate': 'ctrl+a+b', obsolete: 'ctrl+x' });
  await loadKeybindings();
  expect(getKeybindingRules()).toEqual(KEYBINDING_DEFAULTS);
  writeFrontendValue('keybindings', ['ctrl+x']);
  await loadKeybindings();
  expect(getKeybindingRules()).toEqual(KEYBINDING_DEFAULTS);
});

it('keeps the existing choice when local persistence fails', async () => {
  installNativeKeybindings();
  await loadKeybindings();
  const original = getKeybindingRules();
  const setItem = vi.spyOn(localStorage, 'setItem').mockImplementation(() => { throw new Error('storage full'); });
  try {
    await expect(saveKeybindings(withReboundRow(original, original[0], 'ctrl+alt+y'))).rejects.toThrow('could not save');
    expect(getKeybindingRules()).toEqual(original);
  } finally { setItem.mockRestore(); }
});

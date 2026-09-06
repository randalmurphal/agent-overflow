import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { DisconnectedError, TransportError } from '../transport/wsClient';
import { ReadDeadlineError } from '../utils/readBeforeDeadline';
import { isPassiveConnectionFailure } from '../transport/passiveReadFailure';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { resetStagedBackends } from '../../test/helpers/backends';
import { refreshThreads, loadThreads } from './threads.svelte';
import { refreshProjects } from './projects.svelte';
import { refreshThreadGroups } from './threadGroups.svelte';
import { loadSettings } from './settings.svelte';
import { getKeybindingRules, loadKeybindings, resyncKeybindings, saveKeybindings, setKeybindingsForTest } from './keybindings.svelte';
import { getToasts, removeToast } from './toast.svelte';

beforeEach(() => { resetStagedBackends(); for (const toast of [...getToasts()]) removeToast(toast.id); });
afterEach(() => { resetStagedBackends(); vi.useRealTimers(); });

it.each([
  new DisconnectedError('transport unreachable'),
  new DisconnectedError('backend restarted', { terminal: true }),
  new TransportError('auth_failed', 'pairing required'),
  new ReadDeadlineError(),
])('keeps automatic startup/reconnect reads quiet for $name', async (error) => {
  for (const method of ['ListThreads', 'ListProjects', 'ListThreadGroups', 'GetSettings', 'GetKeybindings']) {
    setBindingMock(method, async () => { throw error; });
  }
  await Promise.all([refreshThreads(), refreshProjects(), refreshThreadGroups(), loadSettings(), loadKeybindings()]);
  await refreshThreads();
  expect(getToasts()).toEqual([]);
  // Unknown catalogs must not become a successful empty response: startup
  // needs to retry pane restoration after the connection returns.
  await expect(loadThreads()).rejects.toBe(error);
});

it('retains working shortcuts across a failed read, then replaces them on recovery', async () => {
  const existing = [{ key: 'ctrl+k', command: 'test.command' }];
  setKeybindingsForTest(existing);
  setBindingMock('GetKeybindings', async () => { throw new DisconnectedError(); });
  await resyncKeybindings();
  expect(getKeybindingRules()).toEqual(existing);
  setBindingMock('GetKeybindings', async () => ({ bindings: [{ key: 'ctrl+j', command: 'test.command' }] }));
  await resyncKeybindings();
  expect(getKeybindingRules()[0].key).toBe('ctrl+j');
  expect(getToasts()).toEqual([]);
});

it('does not hide a real backend error or a failed user write', async () => {
  const broken = new TransportError('method_error', 'database read failed');
  expect(isPassiveConnectionFailure(broken)).toBe(false);
  expect(isPassiveConnectionFailure(new TransportError('scope_required', 'not authorized'))).toBe(false);
  expect(isPassiveConnectionFailure(new TransportError('timeout', 'RPC timed out'))).toBe(false);
  setBindingMock('GetSettings', async () => { throw broken; });
  await loadSettings();
  expect(getToasts().map((toast) => toast.message)).toContain('Failed to load settings');
  const disconnected = new DisconnectedError();
  setBindingMock('UpdateKeybindings', async () => { throw disconnected; });
  await expect(saveKeybindings([])).rejects.toBe(disconnected);
});

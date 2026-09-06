import { afterEach, expect, it, vi } from 'vitest';
import { stageBackend, resetStagedBackends } from '../../test/helpers/backends';
import { attachedBackends, attachedBackendCount, backendById, detachBackend, homeBackend, requireEntityBackend, subscribeEveryBackend } from './backends';
import { Call } from './runtime';
import { HOME_BACKEND } from './backendKey';
import { selectedBackend, setSelectedBackend } from '../stores/selectedBackend.svelte';
import { setCarriedSessionScopes } from './scopes';
import { workflowItemHasScope } from './entityScopes';

// setup.ts has already imported the ordinary desktop registry. This fixture
// boots a fresh document in frontend-only mode, just as the native shell does.
vi.hoisted(() => vi.resetModules());
vi.mock('./runMode', () => ({ isFrontendOnly: () => true, isClientMode: () => true, initialComputer: () => 'first' }));

afterEach(() => { resetStagedBackends(); vi.restoreAllMocks(); setSelectedBackend('first'); });

it('uses the sole execution computer for unknown-item permissions, never the controller', () => {
  stageBackend({ id: 'first', backendId: 'first' });
  setCarriedSessionScopes('first', ['threads:read']);
  expect(workflowItemHasScope('threads:read', 'unknown-run')).toBe(true);
  expect(workflowItemHasScope('threads:autonomy', 'unknown-run')).toBe(false);
  detachBackend('first');
  expect(workflowItemHasScope('threads:read', 'unknown-run')).toBe(false);
});

it('keeps local administration outside the computer catalog and every-computer calls', async () => {
  const controller = vi.spyOn(homeBackend().client, 'callByID').mockResolvedValue([]);
  expect(backendById(HOME_BACKEND)).toBe(homeBackend());
  expect(attachedBackends()).toEqual([]);
  expect(selectedBackend()).toBe('first');
  stageBackend({ id: 'first', backendId: 'first' });
  const execution = vi.mocked(backendById('first')!.client.callByID).mockResolvedValue([]);
  expect(attachedBackendCount()).toBe(1);
  expect(requireEntityBackend(undefined)).toBe('first');
  // The old single-computer optimization must not send ListThreads to the
  // local controller just because it owns the page's singleton connection.
  await expect(Call.ByID(1090132042)).resolves.toEqual([]); // ListThreads
  expect(execution).toHaveBeenCalledTimes(1);
  expect(controller).not.toHaveBeenCalled();
  await expect(Call.ByID(130055792)).resolves.toEqual([]); // ListBackends
  expect(controller).toHaveBeenCalledTimes(1);
});

it('forgets the launch computer without losing local administration or another computer', async () => {
  const controller = vi.spyOn(homeBackend().client, 'callByID').mockResolvedValue([]);
  const events = vi.spyOn(homeBackend().client, 'subscribe').mockReturnValue(() => {});
  stageBackend({ id: 'first', backendId: 'first' });
  stageBackend({ id: 'second', backendId: 'second' });
  const other = vi.mocked(backendById('second')!.client.callByID).mockResolvedValue([]);
  const cancel = subscribeEveryBackend('backend:attach', () => {});
  expect(events).toHaveBeenCalledWith('backend:attach', expect.any(Function));
  detachBackend('first');
  expect(attachedBackends().map((entry) => entry.id)).toEqual(['second']);
  expect(backendById(HOME_BACKEND)).toBe(homeBackend());
  // An explicit selection of the removed computer cannot land on another.
  await expect(Call.ByID(320967638, '/project')).rejects.toThrow(); // BrowseDirectory
  expect(other).not.toHaveBeenCalled();
  expect(controller).not.toHaveBeenCalled();
  setSelectedBackend('second');
  await expect(Call.ByID(320967638, '/project')).resolves.toEqual([]);
  expect(other).toHaveBeenCalledTimes(1);
  cancel();
});

// Boot through the real multi-computer catalog and RPC ownership pipeline.
// Two saved addresses of one Mac must not become two conversation owners.
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { prepareNativeShell } from './boot';
import { stageBackend, resetStagedBackends } from '../../test/helpers/backends';
import { makeThread } from '../../test/helpers/chat';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { attachedBackends, backendById, restoreHomeBackend, __setHomeClientForTest } from '../transport/backends';
import { storeBackendEndpoint, __resetHomeEndpointForTest } from '../transport/homeEndpoint';
import { Call } from '../transport/runtime';
import { setBackendIdentityFromBootstrap } from '../transport/backendIdentity';
import { wsClient } from '../transport/wsClient';
import { resolveThreadBackend } from '../transport/entityIndex';
import { loadThreads, getThreads } from '../stores/threads.svelte';
import { refreshProjects, getProjects } from '../stores/projects.svelte';
import type { ProjectWithCounts } from '../types/models';
import { __resetSelectedBackendForTest, selectedBackend, setSelectedBackend } from '../stores/selectedBackend.svelte';

const MAC = '11111111-2222-4333-8444-555555555555';
const GPU = '66666666-7777-4888-8999-aaaaaaaaaaaa';
const LIST_THREADS = 1090132042;
const GET_DRAFT = 875977146;

function savePairing(slot: string, computer: string, endpoint: string): void {
  storeBackendEndpoint(slot, endpoint);
  localStorage.setItem(`agent-overflow:deviceSession${slot ? `:${slot}` : ''}`, JSON.stringify({
    backendId: computer, sessionId: `session-${slot || 'legacy'}`, credential: 'isolated-test-credential',
    expiresAtMs: Date.now() + 60_000,
  }));
}

beforeEach(() => {
  localStorage.clear();
  __resetSelectedBackendForTest();
  vi.stubGlobal('Capacitor', { isNativePlatform: () => true });
});

it.each([false, true])('removes the provisional desktop HOME when the native bridge appears late (saved computer=%s)', (savedComputer) => {
  vi.stubGlobal('Capacitor', { isNativePlatform: () => false });
  // This is the registry state created at module evaluation before Capacitor
  // reports a native platform. Native boot must establish its actual catalog.
  restoreHomeBackend();
  expect(backendById('')).toBeDefined();
  if (savedComputer) {
    savePairing(MAC, MAC, 'https://192.168.1.55:60522');
    stageBackend({ id: MAC, backendId: MAC, name: 'Mac' });
  }
  vi.stubGlobal('Capacitor', { isNativePlatform: () => true });
  expect(prepareNativeShell()).toEqual({ shell: true, paired: savedComputer });
  expect(attachedBackends().map((entry) => entry.id)).toEqual(savedComputer ? [MAC] : []);
  if (savedComputer) expect(selectedBackend()).toBe(MAC);
});

it('keeps the legacy computer selected when another saved host attaches before its canonical UUID', () => {
  savePairing('', MAC, 'https://mac.tail.ts.net');
  savePairing(GPU, GPU, 'https://gpu.tail.ts.net');
  savePairing(MAC, MAC, 'https://192.168.1.55:60522');
  stageBackend({ id: GPU, backendId: GPU, name: 'GPU' });
  stageBackend({ id: MAC, backendId: MAC, name: 'Mac' });
  prepareNativeShell();
  expect(selectedBackend()).toBe(MAC);
  // Subsequent boots cannot replace a healthy explicit selection.
  setSelectedBackend(GPU);
  prepareNativeShell();
  expect(selectedBackend()).toBe(GPU);
});
afterEach(() => {
  localStorage.clear();
  vi.unstubAllGlobals();
  __resetHomeEndpointForTest();
  __setHomeClientForTest(wsClient);
  resetStagedBackends();
});

it.each([false, true])('loads one Mac catalog and routes its draft after native boot (other computer=%s)', async (withGpu) => {
  savePairing('', MAC, 'https://mac.tail.ts.net');
  savePairing(MAC, MAC, 'https://192.168.1.55:60522');
  stageBackend({ id: MAC, backendId: MAC, name: 'Mac' });
  if (withGpu) {
    savePairing(GPU, GPU, 'https://gpu.tail.ts.net');
    stageBackend({ id: GPU, backendId: GPU, name: 'GPU' });
  }
  expect(prepareNativeShell()).toEqual({ shell: true, paired: true });
  const computers = withGpu ? [MAC, GPU] : [MAC];
  expect(attachedBackends().map((entry) => entry.id)).toEqual(computers);

  await verifyCatalogsAndDrafts(computers);
});


async function verifyCatalogsAndDrafts(computers: string[]): Promise<void> {
  const catalogs: string[] = [];
  const drafts: string[] = [];
  for (const computer of computers) {
    vi.mocked(backendById(computer)!.client.callByID).mockImplementation(async (method, args) => {
      if (method === LIST_THREADS) {
        catalogs.push(computer);
        return [makeThread({ id: `${computer}-thread`, projectId: `${computer}-project`, title: computer === MAC ? 'Mac task' : 'GPU task' })];
      }
      if (method === GET_DRAFT) {
        expect(args).toEqual([`${computer}-thread`]);
        drafts.push(computer);
        return { text: `${computer} draft` };
      }
      throw new Error(`Unexpected isolated RPC ${method}`);
    });
  }
  // Keep the real catalog read/ownership admission and runtime routing. Only
  // the generated-binding adapter and the remote socket responses are fake.
  setBindingMock('ListThreads', () => Call.ByID(LIST_THREADS));
  const projects: ProjectWithCounts[] = computers.map((computer) => ({
    project: { id: `${computer}-project`, name: 'repo', path: '/repo', sortPosition: 0,
      createdAt: 0, updatedAt: 0, archived: false }, threadCount: 1,
  }));
  // Projects and threads both use readComputerRows; each callback consumes
  // its own pinned target before returning that computer's share.
  const { takePinnedBackend } = await import('../transport/backends');
  setBindingMock('ListProjects', async () => {
    const computer = takePinnedBackend();
    return projects.filter((row) => row.project.id === `${computer}-project`);
  });
  await refreshProjects();
  await loadThreads();
  expect(getProjects().map((row) => row.project.id)).toEqual(projects.map((row) => row.project.id));
  expect(catalogs).toEqual(computers);
  expect(getThreads().map((thread) => thread.id)).toEqual(computers.map((id) => `${id}-thread`));
  for (const computer of computers) {
    expect(resolveThreadBackend(`${computer}-thread`)).toBe(computer);
    await expect(Call.ByID(GET_DRAFT, `${computer}-thread`)).resolves.toEqual({ text: `${computer} draft` });
  }
  expect(drafts).toEqual(computers);
}


it.each([true, false])('settles delayed legacy HOME identity before catalog reads (complete UUID pairing=%s)', async (completeUuid) => {
  // Old shells saved credentials before the session shape carried backendId.
  savePairing('', '', 'https://mac.tail.ts.net');
  if (completeUuid) savePairing(MAC, MAC, 'https://192.168.1.55:60522');
  else storeBackendEndpoint(MAC, 'https://192.168.1.55:60522');
  stageBackend({ id: MAC, backendId: MAC, name: 'Mac' });
  const uuidEntry = backendById(MAC)!;
  // Both transport handles remain hermetic. Detaching a duplicate calls the
  // fake close, never a real socket or native bridge.
  Object.assign(uuidEntry.client, { setLease: vi.fn(), setWatchedThreads: vi.fn(), setScreenPresence: vi.fn() });
  __setHomeClientForTest(uuidEntry.client);
  expect(prepareNativeShell()).toEqual({ shell: true, paired: true });
  expect(attachedBackends().map((entry) => entry.id)).toEqual(['', MAC]);

  setBackendIdentityFromBootstrap(MAC, 'generation', 'Mac', '');
  const canonical = completeUuid ? MAC : '';
  expect(attachedBackends().map((entry) => entry.id)).toEqual([canonical]);
  expect(backendById(MAC)).toBe(backendById(canonical));
  // A repeated boot must not resurrect the retired descriptor.
  prepareNativeShell();
  expect(attachedBackends().map((entry) => entry.id)).toEqual([canonical]);
  await verifyCatalogsAndDrafts([canonical]);
});

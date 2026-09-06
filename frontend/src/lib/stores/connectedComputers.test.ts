import { beforeEach, afterEach, describe, expect, it, vi } from 'vitest';
import { stageBackend, resetStagedBackends } from '../../test/helpers/backends';
import { grantBackendScopes } from '../../test/helpers/scopes';
import { account, deferred } from '../../test/helpers/providerAccounts';
import { makeSettings } from '../../test/helpers/settings';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { takePinnedBackend } from '../transport/backends';
import { setSelectedBackend } from './selectedBackend.svelte';
import { getSettings, loadSettings, resetSettingsForTest, resyncSettings, updateSetting } from './settings.svelte';
import { getProviderAccount, resetForTest as resetAccounts } from './accountInfo.svelte';
import { getProviderRateLimit, resetForTest as resetLimits } from './rateLimitsInfo.svelte';
import { getProviderModels, refreshProviderModels } from './providerModels.svelte';
import { getProviderAccountsFor, loadProviderAccounts, resetForTest, switchProviderAccount } from './providerAccounts.svelte';
import { applyProviderAccount, applyUsageEvent } from './eventsProvider';
import type { Settings, ModelInfo } from '../types/settings';

const GPU = 'gpu';
const MAC = '';
beforeEach(async () => {
  resetStagedBackends();
  stageBackend({ id: GPU, name: 'GPU' });
  await grantBackendScopes(GPU, ['settings:read', 'settings:write', 'access:admin', 'threads:operate']);
  resetSettingsForTest();
  resetForTest();
  resetAccounts();
  resetLimits();
});
afterEach(() => { resetStagedBackends(); resetLimits(); setSelectedBackend(MAC); });

describe('computer ownership', () => {
  it('keeps frontend preferences through remote echoes and a frontend restart', async () => {
    setBindingMock('GetSettings', async () => makeSettings({ fontSize: takePinnedBackend() === GPU ? 22 : 15 }));
    await loadSettings(MAC);
    await loadSettings(GPU);
    expect(getSettings(GPU).fontSize).toBe(15);
    setBindingMock('UpdateSettings', async () => { throw new Error('offline'); });
    const quiet = vi.spyOn(console, 'error').mockImplementation(() => {});
    await updateSetting('fontSize', 18, GPU);
    await resyncSettings(GPU);
    expect(getSettings(MAC).fontSize).toBe(18);
    expect(getSettings(GPU).fontSize).toBe(18);
    resetSettingsForTest();
    expect(getSettings().fontSize).toBe(18);
    quiet.mockRestore();
  });

  it('saves device preferences immediately while host reads and mirrors are pending', async () => {
    const read = deferred<Partial<Settings>>();
    const mirror = deferred<Partial<Settings>>();
    const sent: Array<{ backend: string; patch: unknown }> = [];
    setBindingMock('GetSettings', () => read.promise);
    setBindingMock('UpdateSettings', (patch: unknown) => {
      sent.push({ backend: takePinnedBackend()!, patch });
      return mirror.promise;
    });
    const loading = loadSettings(MAC);
    await updateSetting('fontSize', 17, GPU);
    expect(getSettings(MAC).fontSize).toBe(17);
    expect(sent).toEqual([
      { backend: MAC, patch: { fontSize: 17 } }, { backend: GPU, patch: { fontSize: 17 } },
    ]);
    // Frontend-only defaults do not overwrite either computer's user settings.
    await updateSetting('defaultThreadEnvMode', 'worktree', GPU);
    expect(sent).toHaveLength(2);
    mirror.resolve(makeSettings());
    read.resolve(makeSettings());
    await loading;
  });

  it('lets a second computer save while the first is slow, without redirecting either', async () => {
    const slow = deferred<Partial<Settings>>();
    const destinations: string[] = [];
    setBindingMock('UpdateSettings', async (patch: unknown) => {
      const backend = takePinnedBackend()!;
      destinations.push(backend);
      if (backend === MAC) return slow.promise;
      return makeSettings(patch as Partial<Settings>);
    });
    const local = updateSetting('claudeBinaryPath', '/mac/claude', MAC);
    setSelectedBackend(MAC);
    await updateSetting('claudeBinaryPath', '/gpu/claude', GPU);
    expect(destinations).toEqual([MAC, GPU]);
    expect(getSettings(GPU).claudeBinaryPath).toBe('/gpu/claude');
    expect(getSettings(MAC).claudeBinaryPath).toBe('/mac/claude');
    slow.resolve(makeSettings({ claudeBinaryPath: '/mac/claude' }));
    await local;
    expect(getSettings(GPU).claudeBinaryPath).toBe('/gpu/claude');
  });

  it('keeps a newer optimistic edit visible while an earlier answer lands', async () => {
    const first = deferred<Partial<Settings>>();
    const second = deferred<Partial<Settings>>();
    let calls = 0;
    setBindingMock('UpdateSettings', () => ++calls === 1 ? first.promise : second.promise);
    const one = updateSetting('claudeBinaryPath', '/first', GPU);
    const two = updateSetting('claudeBinaryPath', '/second', GPU);
    first.resolve(makeSettings({ claudeBinaryPath: '/first' }));
    await one;
    expect(getSettings(GPU).claudeBinaryPath).toBe('/second');
    second.resolve(makeSettings({ claudeBinaryPath: '/second' }));
    await two;
  });

  it('keeps identical account IDs and their quotas separate across computers', async () => {
    setBindingMock('ListProviderAccounts', async () => {
      const remote = takePinnedBackend() === GPU;
      return [account({ id: 'same-id', active: true, email: remote ? 'gpu@example.test' : 'mac@example.test' })];
    });
    await Promise.all([loadProviderAccounts(MAC), loadProviderAccounts(GPU)]);
    for (const [backend, usedPercent] of [[MAC, 10], [GPU, 70]] as const) {
      applyUsageEvent({ action: 'rate_limits', threadId: '', rateLimits: {
        provider: 'claude', accountId: 'same-id', updatedAt: Date.now(), limits: [{
          limitId: 'session', limitName: 'Session', windowMins: 300,
          usedPercent, resetsAt: Date.now() / 1000 + 3600,
        }],
      } }, backend);
    }
    expect(getProviderAccountsFor('claude', GPU)[0].email).toBe('gpu@example.test');
    expect(getProviderAccount('claude', MAC)?.email).toBe('mac@example.test');
    expect(getProviderRateLimit('claude', 300, undefined, GPU)?.usedPercent).toBe(70);
    expect(getProviderRateLimit('claude', 300, undefined, MAC)?.usedPercent).toBe(10);
  });

  it('keeps model catalogs and account-change generations specific to their computer', async () => {
    setBindingMock('GetModelsForProvider', async () => [{ slug: takePinnedBackend() === GPU ? 'gpu-model' : 'mac-model' }] as ModelInfo[]);
    await Promise.all([refreshProviderModels('codex', MAC), refreshProviderModels('codex', GPU)]);
    applyProviderAccount({ provider: 'codex', accountId: 'remote', generation: 100, account: { email: 'gpu@example.test' } }, GPU);
    applyProviderAccount({ provider: 'codex', accountId: 'local', generation: 1, account: { email: 'mac@example.test' } }, MAC);
    expect(getProviderModels('codex', GPU)[0].slug).toBe('gpu-model');
    expect(getProviderModels('codex', MAC)[0].slug).toBe('mac-model');
    expect(getProviderAccount('codex', MAC)?.accountId).toBe('local');
  });

  it('pins the account switch and its subsequent listing despite navigation during the request', async () => {
    const switched = deferred<void>();
    const destinations: string[] = [];
    setBindingMock('SwitchProviderAccount', async () => {
      destinations.push(takePinnedBackend()!);
      await switched.promise;
    });
    setBindingMock('ListProviderAccounts', async () => {
      destinations.push(takePinnedBackend()!);
      return [account({ active: true })];
    });
    const result = switchProviderAccount('claude', account(), GPU);
    setSelectedBackend(MAC);
    switched.resolve();
    await expect(result).resolves.toBe(true);
    expect(destinations).toEqual([GPU, GPU]);
    expect(getProviderAccount('claude', MAC)).toBeNull();
    expect(getProviderAccount('claude', GPU)?.accountId).toBe('acct-1');
  });
});

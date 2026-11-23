// The typed wrappers against a computer on v0.0.19 (test/helpers/
// olderComputer.ts): an action that release has goes through its method,
// a read of a feature it lacks answers empty, and every other failure is
// still the caller's.
import generated from '../../../bindings/agent-overflow/app.ts?raw';
import { beforeEach, describe, expect, it } from 'vitest';
import { getBindingMock, setBindingMock } from '../../test/mocks/bindings-app';
import { answerAsV0019, METHODS_AFTER_V0_0_19, methodNotFound, SETTINGS_AFTER_V0_0_19, settingsAsV0019 } from '../../test/helpers/olderComputer';
import { getPinnedBackend, withBackendTarget } from '../transport/backends';
import { TransportError } from '../transport/wsClient';
import { methodId } from '../transport/methodId';
import { ThreadsPartlyDeleted } from './olderComputer';
import {
  ArchiveThread,
  ArchiveThreadResult,
  CheckAgentProfiles,
  DeleteDependents,
  DeleteThread,
  DeleteThreadResult,
  DeleteThreads,
  GetDeleteDependents,
  DeleteProviderCustomEnvVar,
  GetNativeMemoryStatus,
  GetSettings,
  GetStandingAgent,
  SetProviderCustomEnvVar,
  UpdateSettings,
  GetThreadAgentRows,
  GetThreadRequestStates,
  GetThreadWorkerSummary,
  ListStandingAgentWakes,
  ListThreadAgentRows,
  ListThreadPastAgentRows,
  ListThreadRecurringJobs,
  ListThreadWakes,
  ListThreadWorkerSummaries,
  MarkThreadsRead,
  WorkflowPauseItemAndEndSessions,
} from './bindings';

beforeEach(() => {
  answerAsV0019();
});

describe('the methods v0.0.19 lacks', () => {
  it('are bindings this build has, called by the id the generator gives them', () => {
    for (const name of [...METHODS_AFTER_V0_0_19, 'ArchiveThread', 'DeleteThread']) {
      const match = new RegExp(`^export function ${name}\\([^]*?\\$Call\\.ByID\\((\\d+)`, 'm').exec(generated);
      expect(match, name).not.toBeNull();
      expect(methodId(name), name).toBe(Number(match![1]));
    }
  });
});

describe('archive and delete against v0.0.19', () => {
  it('reads a successful archive answered with null as one that left nothing behind', async () => {
    const result = await ArchiveThread('t1');
    expect(result).toBeInstanceOf(ArchiveThreadResult);
    expect(result.workersNotArchived ?? []).toEqual([]);
    expect(getBindingMock('ArchiveThread')).toHaveBeenCalledWith('t1');
  });

  it('reads a successful delete answered with null as one that left nothing behind', async () => {
    const result = await DeleteThread('t1');
    expect(result).toBeInstanceOf(DeleteThreadResult);
    expect(result.workersNotDeleted ?? []).toEqual([]);
  });

  it('still decodes the result this build answers', async () => {
    setBindingMock('ArchiveThread', async () => ({ workersNotArchived: [{ threadId: 'w', title: 'W', error: 'busy' }] }));
    const result = await ArchiveThread('t1');
    expect(result.workersNotArchived?.[0]?.threadId).toBe('w');
  });

  it('deletes several threads one at a time on the computer the delete was sent to', async () => {
    const pins: (string | null)[] = [];
    setBindingMock('DeleteThread', async () => { pins.push(getPinnedBackend()); return null; });
    const result = await withBackendTarget('laptop', () => DeleteThreads(['a', 'b']));
    expect(result).toBeInstanceOf(DeleteThreadResult);
    expect(getBindingMock('DeleteThreads')).toHaveBeenCalledTimes(1);
    expect(getBindingMock('DeleteThread')!.mock.calls).toEqual([['a'], ['b']]);
    expect(pins).toEqual(['laptop', 'laptop']);
  });

  it('reports which of several threads it could not delete, after deleting the rest', async () => {
    const refusal = new TransportError('method_error', 'Thread b is busy');
    setBindingMock('DeleteThread', async (id: string) => { if (id === 'b') throw refusal; return null; });
    const err = await DeleteThreads(['a', 'b', 'c']).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ThreadsPartlyDeleted);
    expect((err as ThreadsPartlyDeleted).deleted).toEqual(['a', 'c']);
    expect((err as ThreadsPartlyDeleted).failures).toEqual([{ threadId: 'b', error: refusal }]);
  });

  it('fails a delete of one thread with that thread\'s own failure', async () => {
    const refusal = new TransportError('method_error', 'Thread a is busy');
    setBindingMock('DeleteThread', async () => { throw refusal; });
    await expect(DeleteThreads(['a'])).rejects.toBe(refusal);
  });

  it('leaves a failure that is not a missing method to the caller', async () => {
    const refusal = new TransportError('method_error', 'disk full');
    setBindingMock('DeleteThreads', async () => { throw refusal; });
    await expect(DeleteThreads(['a', 'b'])).rejects.toBe(refusal);
    expect(getBindingMock('DeleteThread')).not.toHaveBeenCalled();
  });

  it('answers that a delete takes nothing with it', async () => {
    const counts = await GetDeleteDependents(['a']);
    expect(counts).toBeInstanceOf(DeleteDependents);
    expect(counts).toMatchObject({ pastThreads: 0, helpers: 0, workers: 0, remoteWorkers: false });
  });
});

describe('the other actions v0.0.19 has', () => {
  it('marks threads read one at a time on the same computer', async () => {
    const pins: (string | null)[] = [];
    setBindingMock('MarkThreadRead', async () => { pins.push(getPinnedBackend()); return null; });
    expect(await withBackendTarget('laptop', () => MarkThreadsRead(['a', 'b']))).toEqual([]);
    expect(getBindingMock('MarkThreadRead')!.mock.calls).toEqual([['a'], ['b']]);
    expect(pins).toEqual(['laptop', 'laptop']);
  });

  it('fails Mark All Read with the first thread\'s failure after trying every thread', async () => {
    const refusal = new TransportError('method_error', 'locked');
    setBindingMock('MarkThreadRead', async (id: string) => { if (id === 'a') throw refusal; return null; });
    await expect(MarkThreadsRead(['a', 'b'])).rejects.toBe(refusal);
    expect(getBindingMock('MarkThreadRead')).toHaveBeenCalledTimes(2);
  });

  // That release cannot end sessions; its Pause, on the same row, still pauses.
  it('refuses Pause and End Sessions, saying the computer cannot end sessions', async () => {
    await expect(WorkflowPauseItemAndEndSessions('item-1')).rejects.toThrow(
      'That computer runs an older version, which cannot end sessions. Pause still pauses the run.',
    );
    expect(getBindingMock('WorkflowPauseItem')).not.toHaveBeenCalled();
  });
});

describe('the surfaces v0.0.19 lacks read empty', () => {
  it('answers every read of them with nothing to show', async () => {
    expect((await ListThreadWakes('t1')).rows).toEqual([]);
    expect(await ListStandingAgentWakes()).toEqual([]);
    const page = await ListThreadWorkerSummaries('');
    expect(page.summaries).toEqual([]);
    expect(page.next ?? '').toBe('');
    expect(await GetThreadWorkerSummary('t1')).toMatchObject({ threadId: 't1', open: 0, needYou: 0 });
    expect((await ListThreadAgentRows('t1')).rows).toEqual([]);
    expect(await GetThreadAgentRows('t1', ['k'])).toEqual([]);
    const past = await ListThreadPastAgentRows('t1', { at: 0, id: '' } as never, 20);
    expect(past.rows).toEqual([]);
    expect(past.total).toBe(0);
    expect(await GetThreadRequestStates('t1', ['k'])).toEqual([]);
    expect(await GetNativeMemoryStatus()).toBeNull();
    expect(await CheckAgentProfiles([])).toBeNull();
    expect(await GetStandingAgent('t1')).toBeNull();
    expect(await ListThreadRecurringJobs('t1')).toEqual([]);
  });

  it('leaves any other failure of those reads to the caller', async () => {
    const refusal = new TransportError('timeout', 'timed out');
    setBindingMock('ListThreadAgentRows', async () => { throw refusal; });
    await expect(ListThreadAgentRows('t1')).rejects.toBe(refusal);
    setBindingMock('GetDeleteDependents', async () => { throw refusal; });
    await expect(GetDeleteDependents(['a'])).rejects.toBe(refusal);
    setBindingMock('CheckAgentProfiles', async () => { throw methodNotFound(); });
    await expect(CheckAgentProfiles([])).resolves.toBeNull();
  });
});

// A settings answer reaches the store as the computer sent it: the
// generated Settings class would give each key v0.0.19 lacks a zero value
// and hide that it lacks it (settings.svelte.ts, computerHasSetting).
describe('settings answers from a computer on v0.0.19', () => {
  it('keep out every key that release does not send', async () => {
    for (const name of ['GetSettings', 'UpdateSettings', 'SetProviderCustomEnvVar', 'DeleteProviderCustomEnvVar']) {
      setBindingMock(name, async () => settingsAsV0019());
    }
    const answers = [
      await GetSettings(), await UpdateSettings({ claudeEnabled: true }),
      await SetProviderCustomEnvVar('claude', 'A', 'b', false), await DeleteProviderCustomEnvVar('claude', 'A'),
    ];
    for (const answer of answers) {
      for (const key of SETTINGS_AFTER_V0_0_19) expect(answer, key).not.toHaveProperty(key);
      expect(answer).toHaveProperty('claudeEnabled');
    }
  });
});

import { beforeEach, describe, expect, it, vi } from 'vitest';
import { resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';
import { makeSettings } from '../../test/helpers/settings';
import { loadSettingsFixture as loadSettings } from '../../test/helpers/settingsFixture';
import { autoPinNewThread, shouldAutoPinFirstSend } from './threadAutoPin';
import { getToasts, removeToast } from './toast.svelte';
import type { Thread } from '../types/models';

function mkThread(overrides: Partial<Thread> = {}): Thread {
  return {
    id: 't1',
    title: 'New thread',
    provider: 'claude',
    projectId: 'project-1',
    workspacePath: '/tmp/work',
    projectPath: '/tmp/work',
    model: 'claude-sonnet-4-6',
    createdAt: 0,
    updatedAt: 0,
    archived: false,
    isDraft: true,
    ...overrides,
  };
}

describe('shouldAutoPinFirstSend', () => {
  beforeEach(async () => {
    resetBindingMocks();
    setBindingMock('GetSettings', async () => makeSettings({ autoPinNewThreads: true }));
    await loadSettings();
  });

  it('pins a plain in-app draft on its first send', () => {
    expect(shouldAutoPinFirstSend(mkThread())).toBe(true);
  });


  it('still refuses imports, children, hidden modes, and non-drafts', () => {
    expect(shouldAutoPinFirstSend(undefined)).toBe(false);
    expect(shouldAutoPinFirstSend(mkThread({ isDraft: false }))).toBe(false);
    expect(shouldAutoPinFirstSend(mkThread({ importSource: 'claude' }))).toBe(false);
    expect(shouldAutoPinFirstSend(mkThread({ parentThreadId: 'root' }))).toBe(false);
    expect(shouldAutoPinFirstSend(mkThread({ mode: 'workflow' }))).toBe(false);
  });

  it('respects the setting being off', async () => {
    setBindingMock('GetSettings', async () => makeSettings({ autoPinNewThreads: false }));
    await loadSettings();
    expect(shouldAutoPinFirstSend(mkThread())).toBe(false);
  });
});

describe('autoPinNewThread', () => {
  beforeEach(async () => {
    resetBindingMocks();
    for (const toast of [...getToasts()]) removeToast(toast.id);
    setBindingMock('GetSettings', async () => makeSettings({ autoPinNewThreads: true }));
    await loadSettings();
  });

  it('pins through the RPC and returns the pinned row', async () => {
    const pin = vi.fn(async (id: string) => mkThread({ id, isDraft: false, pinnedAt: 5 }));
    setBindingMock('PinThread', pin);
    const out = await autoPinNewThread(mkThread({ isDraft: false }));
    expect(pin).toHaveBeenCalledWith('t1');
    expect(out.pinnedAt).toBe(5);
  });

  it('never pins a grouped thread, and never toasts for one', async () => {
    // A fork inherits its source's group (BuildForkedThread) and reaches
    // this without the first-send pre-check; the store CHECK refuses a pin
    // on a grouped row, and the user was seeing that as an error toast on
    // every fork inside a group (2026-09-02).
    const pin = vi.fn(async () => { throw new Error('a grouped thread cannot be pinned'); });
    setBindingMock('PinThread', pin);
    const grouped = mkThread({ isDraft: false, groupId: 'g1' });
    expect(await autoPinNewThread(grouped)).toBe(grouped);
    expect(pin).not.toHaveBeenCalled();
    expect(getToasts()).toEqual([]);
  });

  it('surfaces a pin failure as a toast and returns the thread unchanged', async () => {
    setBindingMock('PinThread', async () => { throw new Error('boom'); });
    const t = mkThread({ isDraft: false });
    expect(await autoPinNewThread(t)).toBe(t);
    expect(getToasts().map((x) => x.type)).toEqual(['error']);
  });
});

import { describe, expect, it, beforeEach, vi } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import DiscussionStartFlow from './DiscussionStartFlow.svelte';
import { getAllPanes } from '../../stores/panes.svelte';
import { createThreadPane } from '../../stores/thread.svelte';
import { loadSettings } from '../../stores/settings.svelte';
import type { Thread } from '../../types/models';
import type { DiscussionDefinition } from '../../types/discussion';
import { setBindingMock } from '../../../test/mocks/bindings-app';

// Stub Element.animate so Svelte transitions in the modal don't throw.
if (typeof Element !== 'undefined' && !('animate' in Element.prototype)) {
  (Element.prototype as unknown as { animate: unknown }).animate = function () {
    return {
      cancel() {}, finish() {}, play() {}, pause() {}, reverse() {},
      addEventListener() {}, removeEventListener() {},
      onfinish: null, oncancel: null, finished: Promise.resolve(),
      effect: null, startTime: 0, currentTime: 0, playState: 'finished', playbackRate: 1,
    };
  };
}

function makeThread(overrides: Partial<Thread> = {}): Thread {
  return {
    id: 'parent',
    title: 'Candidate thread',
    provider: 'claude',
    workspacePath: '/workspace',
    projectPath: '/project',
    mode: 'chat',
    model: 'claude-sonnet-4-6',
    createdAt: 0,
    updatedAt: 0,
    archived: false,
    ...overrides,
  };
}

function makeDef(overrides: Partial<DiscussionDefinition> = {}): DiscussionDefinition {
  return {
    id: 'd1',
    name: 'interrogate',
    description: 'advocate/interrogator',
    scope: 'global',
    projectId: '',
    participants: [
      { role: 'advocate', description: '', system: 'stand firm', provider: undefined, model: undefined },
      { role: 'interrogator', description: '', system: 'press harder', provider: undefined, model: undefined },
    ],
    settings: { maxTurns: 6 },
    createdAt: 0,
    updatedAt: 0,
    ...overrides,
  };
}

async function buildPane(thread = makeThread()) {
  setBindingMock('SwitchThread', async () => {});
  setBindingMock('ListItems', async () => []);
  setBindingMock('ListPayloadMetas', async () => []);
  const pane = createThreadPane();
  await pane.switchThread(thread);
  getAllPanes().set('main', pane);
  return pane;
}

describe('<DiscussionStartFlow>', () => {
  beforeEach(async () => {
    setBindingMock('GetSettings', async () => null);
    await loadSettings();
  });

  it('loads project and global discussions on open and lists them (project first)', async () => {
    const pane = await buildPane(makeThread({ projectPath: '/project-a' }));
    const listMock = setBindingMock('ListDiscussions', async (scope: string) => {
      if (scope === 'project') {
        return [makeDef({ id: 'p1', name: 'code-review', scope: 'project', projectId: '/project-a' })];
      }
      return [makeDef({ id: 'g1', name: 'interrogate' })];
    });

    const { findAllByRole } = render(DiscussionStartFlow, {
      props: {
        open: true,
        thread: pane.thread!,
        pane,
        onClose: () => {},
      },
    });

    for (let i = 0; i < 5; i++) await Promise.resolve();
    const options = await findAllByRole('option');
    expect(options.length).toBe(2);
    // Project-scoped first.
    expect(options[0].textContent).toMatch(/code-review/);
    expect(options[1].textContent).toMatch(/interrogate/);
    expect(listMock.mock.calls.length).toBeGreaterThanOrEqual(2);
  });

  it('calls StartDiscussion and closes when the user confirms', async () => {
    const pane = await buildPane();
    setBindingMock('ListDiscussions', async (scope: string) => {
      if (scope === 'global') return [makeDef({ id: 'g1', name: 'interrogate' })];
      return [];
    });
    const startMock = setBindingMock('StartDiscussion', async () => {});
    // GetThread is called after successful start to refresh the parent thread.
    setBindingMock('GetThread', async () => ({
      ...pane.thread!,
      mode: 'discussion',
      discussionId: 'channel-new',
    }));
    let closedCount = 0;
    const { findByRole } = render(DiscussionStartFlow, {
      props: {
        open: true,
        thread: pane.thread!,
        pane,
        onClose: () => { closedCount++; },
      },
    });
    // Let list load.
    for (let i = 0; i < 5; i++) await Promise.resolve();
    const startBtn = await findByRole('button', { name: /^start$/i });
    await fireEvent.click(startBtn);
    for (let i = 0; i < 5; i++) await Promise.resolve();
    expect(startMock.mock.calls[0]).toEqual(['parent', 'interrogate']);
    expect(closedCount).toBe(1);
    expect(pane.thread?.mode).toBe('discussion');
    expect(pane.thread?.discussionId).toBe('channel-new');
  });

  it('surfaces an inline error when StartDiscussion rejects and does not close', async () => {
    const pane = await buildPane();
    setBindingMock('ListDiscussions', async (scope: string) => {
      if (scope === 'global') return [makeDef({ id: 'g1', name: 'interrogate' })];
      return [];
    });
    setBindingMock('StartDiscussion', async () => { throw new Error('thread already has participants'); });
    const consoleErr = vi.spyOn(console, 'error').mockImplementation(() => {});

    let closedCount = 0;
    const { findByRole, getAllByRole } = render(DiscussionStartFlow, {
      props: {
        open: true,
        thread: pane.thread!,
        pane,
        onClose: () => { closedCount++; },
      },
    });
    for (let i = 0; i < 5; i++) await Promise.resolve();

    const startBtn = await findByRole('button', { name: /^start$/i });
    await fireEvent.click(startBtn);
    for (let i = 0; i < 5; i++) await Promise.resolve();

    const alerts = getAllByRole('alert');
    const combined = alerts.map((a) => a.textContent ?? '').join(' ');
    expect(combined).toMatch(/thread already has participants/i);
    expect(closedCount).toBe(0);
    consoleErr.mockRestore();
  });

  it('selecting a discussion updates which one Start will launch', async () => {
    const pane = await buildPane();
    setBindingMock('ListDiscussions', async (scope: string) => {
      if (scope === 'global') {
        return [
          makeDef({ id: 'g1', name: 'interrogate' }),
          makeDef({ id: 'g2', name: 'code-review' }),
        ];
      }
      return [];
    });
    const startMock = setBindingMock('StartDiscussion', async () => {});
    setBindingMock('GetThread', async () => pane.thread!);

    const { findAllByRole, findByRole } = render(DiscussionStartFlow, {
      props: {
        open: true,
        thread: pane.thread!,
        pane,
        onClose: () => {},
      },
    });
    for (let i = 0; i < 5; i++) await Promise.resolve();

    const options = await findAllByRole('option');
    expect(options.length).toBe(2);
    // Select the second one explicitly.
    await fireEvent.click(options[1]);
    for (let i = 0; i < 3; i++) await Promise.resolve();

    const startBtn = await findByRole('button', { name: /^start$/i });
    await fireEvent.click(startBtn);
    for (let i = 0; i < 5; i++) await Promise.resolve();
    expect(startMock.mock.calls[0]).toEqual(['parent', 'code-review']);
  });
});

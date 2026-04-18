import { beforeEach, describe, expect, it } from 'vitest';
import { render } from '@testing-library/svelte';

import BelowComposerBar from './BelowComposerBar.svelte';
import { createThreadPane } from '../../../stores/thread.svelte';
import type { Thread } from '../../../types/models';
import {
  resetBindingMocks,
  setBindingMock,
} from '../../../../test/mocks/bindings-app';

function makeThread(overrides: Partial<Thread> = {}): Thread {
  return {
    id: 'thread-1',
    title: 'Test',
    provider: 'claude',
    workspacePath: '/repo',
    projectPath: '/repo',
    mode: 'chat',
    model: 'm',
    createdAt: 0,
    updatedAt: 0,
    archived: false,
    ...overrides,
  };
}

async function buildPane(thread: Thread | null) {
  setBindingMock('SwitchThread', async () => {});
  setBindingMock('ListItems', async () => []);
  setBindingMock('ListPayloadMetas', async () => []);
  const pane = createThreadPane();
  if (thread) await pane.switchThread(thread);
  return pane;
}

describe('<BelowComposerBar>', () => {
  beforeEach(() => resetBindingMocks());

  it('renders nothing when no active thread', async () => {
    const pane = await buildPane(null);
    const { queryByTestId } = render(BelowComposerBar, { props: { pane } });
    expect(queryByTestId('below-composer-bar')).toBeNull();
  });

  it('renders EnvPicker and BranchPicker when a thread is active', async () => {
    const pane = await buildPane(makeThread({ branch: 'main' }));
    const { getByTestId } = render(BelowComposerBar, { props: { pane } });
    expect(getByTestId('below-composer-bar')).toBeTruthy();
    expect(getByTestId('env-picker-trigger')).toBeTruthy();
    expect(getByTestId('branch-picker-trigger')).toBeTruthy();
  });
});

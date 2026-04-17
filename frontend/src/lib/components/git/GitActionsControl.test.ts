import { describe, expect, it, beforeEach } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import { tick } from 'svelte';
import GitActionsControl from './GitActionsControl.svelte';
import { createThreadPane } from '../../stores/thread.svelte';
import { loadSettings } from '../../stores/settings.svelte';
import type { GitStatus } from '../../types/git';
import type { Thread } from '../../types/models';
import { setBindingMock } from '../../../test/mocks/bindings-app';

// Svelte transitions poke Element.animate on mount; jsdom lacks it.
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
    id: 'thread-1',
    title: 'Example',
    provider: 'claude',
    workspacePath: '/workspace',
    projectPath: '/workspace',
    model: 'claude-sonnet-4-6',
    interactionMode: 'default',
    createdAt: 0,
    updatedAt: 0,
    archived: false,
    ...overrides,
  };
}

function status(overrides: Partial<GitStatus> = {}): GitStatus {
  return {
    isRepo: true,
    branch: 'main',
    isDefaultBranch: true,
    hasChanges: false,
    insertions: 0,
    deletions: 0,
    fileCount: 0,
    hasUpstream: true,
    aheadCount: 0,
    behindCount: 0,
    hasOriginRemote: true,
    ...overrides,
  };
}

async function buildPane(thread = makeThread()) {
  setBindingMock('SwitchThread', async () => {});
  setBindingMock('ListItems', async () => []);
  setBindingMock('ListPayloadMetas', async () => []);
  const pane = createThreadPane();
  await pane.switchThread(thread);
  return pane;
}

async function flush(n = 8): Promise<void> {
  for (let i = 0; i < n; i += 1) await tick();
}

describe('<GitActionsControl> repo gating (Bug C1)', () => {
  beforeEach(async () => {
    setBindingMock('GetSettings', async () => null);
    setBindingMock('GetProviderStatuses', async () => []);
    await loadSettings();
  });

  it('renders nothing when the workspace is not a git repo', async () => {
    const pane = await buildPane();
    // Critical scenario: GetGitStatus succeeds but signals "not a repo" via
    // IsRepo=false. Before the fix the `{:else if status}` branch rendered
    // the full menu including Ship Changes, which would then fire git RPCs
    // against a non-git directory.
    setBindingMock('GetGitStatus', async () => status({ isRepo: false, branch: '' }));
    const { queryByTestId, container } = render(GitActionsControl, { props: { pane } });
    await flush();
    expect(queryByTestId('git-actions-ship')).toBeNull();
    expect(queryByTestId('git-actions-error')).toBeNull();
    // Control renders no primary action button either.
    expect(container.querySelector('button[aria-label="More git actions"]')).toBeNull();
  });

  it('shows the retry affordance when GetGitStatus rejects', async () => {
    const pane = await buildPane();
    setBindingMock('GetGitStatus', async () => { throw new Error('ENOENT git'); });
    const { findByTestId } = render(GitActionsControl, { props: { pane } });
    const errorButton = await findByTestId('git-actions-error');
    expect(errorButton).toBeInTheDocument();
  });

  it('renders the Ship Changes menu entry in a valid repo', async () => {
    const pane = await buildPane();
    setBindingMock('GetGitStatus', async () => status({ isRepo: true, hasChanges: true }));
    const { container, queryByTestId } = render(GitActionsControl, { props: { pane } });
    await flush();

    // We're not in the error state.
    expect(queryByTestId('git-actions-error')).toBeNull();

    // Open the dropdown to expose the Ship Changes entry.
    const trigger = container.querySelector<HTMLButtonElement>('button[aria-label="More git actions"]');
    expect(trigger).not.toBeNull();
    await fireEvent.click(trigger!);
    await flush();
    expect(queryByTestId('git-actions-ship')).not.toBeNull();
  });
});

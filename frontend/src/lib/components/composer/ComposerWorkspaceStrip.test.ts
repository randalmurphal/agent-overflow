import { beforeEach, describe, expect, it } from 'vitest';
import { fireEvent, render } from '@testing-library/svelte';
import { tick } from 'svelte';

import ComposerWorkspaceStrip from './ComposerWorkspaceStrip.svelte';
import { buildPane, makeThread } from '../../../test/helpers/chat';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import {
  enterCreateBranchMode,
  resetForTest as resetWorktreeIntent,
  setThreadEnvMode,
  worktreeIntentForThread,
} from '../../stores/worktreeIntent.svelte';
import { idleWorkspaceActivity } from '../../../test/helpers/workspaceLock';

describe('<ComposerWorkspaceStrip>', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetWorktreeIntent();
    setBindingMock('ListLiveBackgroundTasks', async () => []);
    setBindingMock('GetWorkspaceActivity', async () => idleWorkspaceActivity());
    setBindingMock('GitListBranches', async () => []);
  });

  it('renders the env and branch pickers when a thread is active', async () => {
    const pane = await buildPane(makeThread());
    const { getByTestId } = render(ComposerWorkspaceStrip, { props: { pane } });
    expect(getByTestId('composer-workspace-strip')).toBeInTheDocument();
    expect(getByTestId('env-picker-trigger')).toBeInTheDocument();
    expect(getByTestId('branch-picker-trigger')).toBeInTheDocument();
  });

  it('hides the env and branch pickers on a design thread', async () => {
    // Design threads operate against the project root with no
    // worktree/branch surface to switch — so the strip stops after the
    // project picker. Mode is post-creation-immutable, so a fixture
    // with mode=design is sufficient (no flip path to test).
    const pane = await buildPane(makeThread({ mode: 'design' }));
    const { getByTestId, queryByTestId } = render(ComposerWorkspaceStrip, { props: { pane } });
    expect(getByTestId('composer-workspace-strip')).toBeInTheDocument();
    expect(getByTestId('thread-mode-picker-trigger')).toBeInTheDocument();
    expect(queryByTestId('env-picker-trigger')).toBeNull();
    expect(queryByTestId('branch-picker-trigger')).toBeNull();
  });

  it('renders thread mode picker before env and branch pickers in DOM order', async () => {
    // Thread mode leads, then env (worktree), then branch — all
    // on the left so the strip reads as a single "where am I" group.
    // (The project picker also renders here when a projectId is set
    // on the thread; this fixture intentionally omits it.) A revert
    // or accidental re-order would otherwise sail past the
    // existence-only assertion above.
    const pane = await buildPane(makeThread());
    const { getByTestId } = render(ComposerWorkspaceStrip, { props: { pane } });
    const strip = getByTestId('composer-workspace-strip');
    const triggers = Array.from(
      strip.querySelectorAll<HTMLElement>('[data-testid$="-picker-trigger"]'),
    );
    expect(triggers.map((el) => el.getAttribute('data-testid'))).toEqual([
      'thread-mode-picker-trigger',
      'env-picker-trigger',
      'branch-picker-trigger',
    ]);
  });

  it('renders the "+ new branch" toggle when intent flips to "new-worktree" and the input only after entering creating-branch mode', async () => {
    // Two-step disclosure: picking "New worktree" surfaces the toggle
    // adjacent to the BranchPicker so the user can opt into creating a
    // new branch; entering creating-branch mode (via the toggle, or
    // via the BranchPicker dropdown's "+ New branch" entry) is what
    // turns the slot into the actual text input.
    const thread = makeThread();
    const pane = await buildPane(thread);
    const { queryByTestId, findByTestId } = render(ComposerWorkspaceStrip, { props: { pane } });
    expect(queryByTestId('worktree-branch-name-input')).toBeNull();
    expect(queryByTestId('new-branch-toggle')).toBeNull();

    setThreadEnvMode(thread, 'new-worktree');
    await tick();
    expect(await findByTestId('new-branch-toggle')).toBeInTheDocument();
    expect(queryByTestId('worktree-branch-name-input')).toBeNull();

    enterCreateBranchMode(thread, { workspaceDirty: false, currentBranch: 'main' });
    await tick();
    expect(await findByTestId('worktree-branch-name-input')).toBeInTheDocument();
    expect(await findByTestId('cancel-new-branch-button')).toBeInTheDocument();
    expect(queryByTestId('new-branch-toggle')).toBeNull();
  });

  it('exits new-branch mode from the input x button and Escape key', async () => {
    const thread = makeThread();
    const pane = await buildPane(thread);
    const { findByTestId, queryByTestId } = render(ComposerWorkspaceStrip, { props: { pane } });

    setThreadEnvMode(thread, 'new-worktree');
    enterCreateBranchMode(thread, { workspaceDirty: false, currentBranch: 'main' });
    await tick();

    await fireEvent.click(await findByTestId('cancel-new-branch-button'));
    expect(worktreeIntentForThread(thread).creatingBranch).toBe(false);
    expect(queryByTestId('worktree-branch-name-input')).toBeNull();

    enterCreateBranchMode(thread, { workspaceDirty: false, currentBranch: 'main' });
    await tick();

    await fireEvent.keyDown(await findByTestId('worktree-branch-name-input'), { key: 'Escape' });
    expect(worktreeIntentForThread(thread).creatingBranch).toBe(false);
    expect(queryByTestId('worktree-branch-name-input')).toBeNull();
  });
});

import { beforeEach, describe, expect, it } from 'vitest';
import { render } from '@testing-library/svelte';
import { tick } from 'svelte';

import ComposerWorkspaceStrip from './ComposerWorkspaceStrip.svelte';
import { buildPane, makeThread } from '../../../test/helpers/chat';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { resetForTest as resetWorktreeIntent, setThreadEnvMode } from '../../stores/worktreeIntent.svelte';

describe('<ComposerWorkspaceStrip>', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetWorktreeIntent();
    setBindingMock('ListLiveBackgroundTasks', async () => []);
    setBindingMock('GitListBranches', async () => []);
  });

  it('renders the env and branch pickers when a thread is active', async () => {
    const pane = await buildPane(makeThread());
    const { getByTestId } = render(ComposerWorkspaceStrip, { props: { pane } });
    expect(getByTestId('composer-workspace-strip')).toBeInTheDocument();
    expect(getByTestId('env-picker-trigger')).toBeInTheDocument();
    expect(getByTestId('branch-picker-trigger')).toBeInTheDocument();
  });

  it('renders env picker before branch picker in DOM order', async () => {
    // Env (worktree) leads, branch follows — both on the left so the
    // strip reads as a single "where am I" group. A revert or
    // accidental re-order would otherwise sail past the existence-only
    // assertion above.
    const pane = await buildPane(makeThread());
    const { getByTestId } = render(ComposerWorkspaceStrip, { props: { pane } });
    const strip = getByTestId('composer-workspace-strip');
    const triggers = Array.from(
      strip.querySelectorAll<HTMLElement>('[data-testid$="-picker-trigger"]'),
    );
    expect(triggers.map((el) => el.getAttribute('data-testid'))).toEqual([
      'env-picker-trigger',
      'branch-picker-trigger',
    ]);
  });

  it('renders the worktree branch-name input only when worktree intent is "new-worktree"', async () => {
    const thread = makeThread();
    const pane = await buildPane(thread);
    const { queryByTestId, findByTestId } = render(ComposerWorkspaceStrip, { props: { pane } });
    expect(queryByTestId('worktree-branch-name-input')).toBeNull();

    setThreadEnvMode(thread, 'new-worktree');
    await tick();
    expect(await findByTestId('worktree-branch-name-input')).toBeInTheDocument();
  });
});

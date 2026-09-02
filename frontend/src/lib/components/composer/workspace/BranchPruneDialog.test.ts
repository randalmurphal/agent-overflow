import { beforeEach, describe, expect, it } from 'vitest';
import { fireEvent, render, waitFor } from '@testing-library/svelte';

import BranchPruneDialog from './BranchPruneDialog.svelte';
import {
  getBindingMock,
  resetBindingMocks,
  setBindingMock,
} from '../../../../test/mocks/bindings-app';

const SAFE = {
  branch: 'merged-gone',
  tip: 'a'.repeat(40),
  subject: 'merged work',
  safe: true,
  reason: 'merged into the default branch',
};
const RISKY = {
  branch: 'maybe-unpushed',
  tip: 'b'.repeat(40),
  subject: 'wip',
  safe: false,
  reason: 'has commits not on the default branch and no matching merged PR — may hold unpushed work',
};

const PRUNE_WS = { projectId: 'project-1', workspacePath: '/workspace' };

function renderDialog() {
  let closed = false;
  const utils = render(BranchPruneDialog, {
    props: {
      workspace: PRUNE_WS,
      open: true,
      onClose: () => {
        closed = true;
      },
    },
  });
  return { ...utils, wasClosed: () => closed };
}

describe('<BranchPruneDialog>', () => {
  beforeEach(() => {
    resetBindingMocks();
  });

  it('pre-checks safe rows only and counts them in the confirm button', async () => {
    setBindingMock('GitListBranchPruneCandidates', async () => ({
      candidates: [SAFE, RISKY],
    }));
    const { findByTestId } = renderDialog();
    await findByTestId('prune-dialog-list');

    const safeBox = (await findByTestId(`prune-row-${SAFE.branch}`)).querySelector('input')!;
    const riskyBox = (await findByTestId(`prune-row-${RISKY.branch}`)).querySelector('input')!;
    expect(safeBox.checked).toBe(true);
    expect(riskyBox.checked).toBe(false);
    const confirm = await findByTestId('prune-dialog-confirm');
    expect(confirm.textContent).toContain('Delete 1 branch');
  });

  it('deletes exactly the checked (branch, tip) pairs and closes on full success', async () => {
    setBindingMock('GitListBranchPruneCandidates', async () => ({
      candidates: [SAFE, RISKY],
    }));
    setBindingMock('GitPruneBranches', async () => ({ deleted: [SAFE.branch] }));

    const { findByTestId, wasClosed } = renderDialog();
    await findByTestId('prune-dialog-list');
    await fireEvent.click(await findByTestId('prune-dialog-confirm'));

    await waitFor(() => {
      expect(getBindingMock('GitPruneBranches')!.mock.calls[0]).toEqual([
        PRUNE_WS,
        [{ branch: SAFE.branch, tip: SAFE.tip }],
      ]);
      expect(wasClosed()).toBe(true);
    });
  });

  it('lets the user opt a risky row in', async () => {
    setBindingMock('GitListBranchPruneCandidates', async () => ({
      candidates: [SAFE, RISKY],
    }));
    setBindingMock('GitPruneBranches', async () => ({
      deleted: [SAFE.branch, RISKY.branch],
    }));

    const { findByTestId } = renderDialog();
    await findByTestId('prune-dialog-list');
    const riskyBox = (await findByTestId(`prune-row-${RISKY.branch}`)).querySelector('input')!;
    await fireEvent.click(riskyBox);
    const confirm = await findByTestId('prune-dialog-confirm');
    expect(confirm.textContent).toContain('Delete 2 branches');
    await fireEvent.click(confirm);

    await waitFor(() => {
      expect(getBindingMock('GitPruneBranches')!.mock.calls[0]).toEqual([
        PRUNE_WS,
        [
          { branch: SAFE.branch, tip: SAFE.tip },
          { branch: RISKY.branch, tip: RISKY.tip },
        ],
      ]);
    });
  });

  it('stays open, re-fetches, and renders per-branch reasons when deletions are refused', async () => {
    setBindingMock('GitListBranchPruneCandidates', async () => ({
      candidates: [SAFE],
    }));
    setBindingMock('GitPruneBranches', async () => ({
      deleted: [],
      failed: { [SAFE.branch]: 'branch changed since the preview; refresh and re-check' },
    }));

    const { findByTestId, wasClosed } = renderDialog();
    await findByTestId('prune-dialog-list');
    await fireEvent.click(await findByTestId('prune-dialog-confirm'));

    await waitFor(() => {
      expect(getBindingMock('GitListBranchPruneCandidates')!.mock.calls.length).toBe(2);
    });
    const failuresBlock = await findByTestId('prune-dialog-failures');
    expect(failuresBlock.textContent).toContain(SAFE.branch);
    expect(failuresBlock.textContent).toContain('changed since the preview');
    expect(wasClosed()).toBe(false);
  });

  it('shows the empty state when nothing qualifies', async () => {
    setBindingMock('GitListBranchPruneCandidates', async () => ({ candidates: [] }));
    const { findByTestId } = renderDialog();
    await findByTestId('prune-dialog-empty');
  });

  it('surfaces the forge warning on the list', async () => {
    setBindingMock('GitListBranchPruneCandidates', async () => ({
      candidates: [RISKY],
      forgeWarning: 'merged PR lookup unavailable: gh missing',
    }));
    const { findByTestId } = renderDialog();
    const warning = await findByTestId('prune-dialog-forge-warning');
    expect(warning.textContent).toContain('gh missing');
  });

  it('surfaces a candidate-listing failure inline', async () => {
    setBindingMock('GitListBranchPruneCandidates', async () => {
      throw new Error('prune preview failed: network unreachable');
    });
    const { findByTestId } = renderDialog();
    const error = await findByTestId('prune-dialog-error');
    expect(error.textContent).toContain('unreachable');
  });
});

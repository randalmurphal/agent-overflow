import { beforeEach, describe, expect, it, vi } from 'vitest';
import { render, fireEvent, waitFor } from '@testing-library/svelte';
import CreatePRDialog from './CreatePRDialog.svelte';
import { resetPanesForTest } from '../../stores/panes.svelte';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { __seedGitStatusForTest } from '../../stores/gitStatusStore.svelte';
import { getToasts, removeToast } from '../../stores/toast.svelte';
import type { GitActionResult, GitStatus } from '../../types/git';
import { buildPane, makeThread } from '../../../test/helpers/chat';

// Partial mock: only the snapshot is pinned (see CommitDialog.test.ts).
vi.mock('../../stores/transportStatus.svelte', async (importOriginal) => ({
  ...(await importOriginal<typeof import('../../stores/transportStatus.svelte')>()),
  getTransportStatus: () => ({ status: 'connected', nextAttemptAt: null }),
}));

// makeThread's default checkout, which is the git-status store key the
// dialog reads its forge through.
const WORKSPACE = '/tmp/workspace';

function status(overrides: Partial<GitStatus> = {}): GitStatus {
  return {
    isRepo: true,
    branch: 'feature',
    isDefaultBranch: false,
    hasChanges: false,
    insertions: 0,
    deletions: 0,
    fileCount: 0,
    hasUpstream: true,
    aheadCount: 0,
    behindCount: 0,
    hasOriginRemote: true,
    forge: 'github',
    ...overrides,
  };
}

function fields() {
  return {
    title: document.getElementById('create-pr-title') as HTMLInputElement,
    body: document.getElementById('create-pr-body') as HTMLTextAreaElement,
    draft: document.getElementById('create-pr-draft') as HTMLInputElement,
  };
}

async function mount(s: GitStatus = status()) {
  const pane = await buildPane(makeThread());
  __seedGitStatusForTest(WORKSPACE, s);
  const onClose = vi.fn();
  const rendered = render(CreatePRDialog, { props: { pane, open: true, onClose } });
  return { pane, onClose, ...rendered };
}

describe('<CreatePRDialog>', () => {
  beforeEach(() => {
    resetPanesForTest();
    resetBindingMocks();
    for (const toast of getToasts()) removeToast(toast.id);
  });

  it('opens with empty fields, draft unchecked, and submit disabled', async () => {
    const { getByTestId, getByText } = await mount(status({ branch: 'feature/login' }));
    const { title, body, draft } = fields();
    expect(title.value).toBe('');
    expect(body.value).toBe('');
    expect(draft.checked).toBe(false);
    expect((getByTestId('create-pr-submit') as HTMLButtonElement).disabled).toBe(true);
    expect(getByText('Create Pull Request')).toBeInTheDocument();
    expect(getByTestId('create-pr-submit').textContent?.trim()).toBe('Create PR');
  });

  it('uses merge request labels for a gitlab forge', async () => {
    const { getByTestId, getByText } = await mount(status({ forge: 'gitlab' }));
    expect(getByText('Create Merge Request')).toBeInTheDocument();
    expect(getByTestId('create-pr-submit').textContent?.trim()).toBe('Create MR');
    expect(fields().title.placeholder).toBe('MR title');
  });

  it('submits the trimmed title, body and draft flag, toasts the URL and closes', async () => {
    const create = setBindingMock('GitCreatePR', async () => ({
      action: 'pr',
      prUrl: 'https://github.com/o/r/pull/7',
    } as GitActionResult));
    const { pane, onClose, getByTestId } = await mount();
    const { title, body, draft } = fields();
    await fireEvent.input(title, { target: { value: '  Add login  ' } });
    await fireEvent.input(body, { target: { value: 'Supports SSO.\n' } });
    await fireEvent.click(draft);
    expect((getByTestId('create-pr-submit') as HTMLButtonElement).disabled).toBe(false);

    await fireEvent.click(getByTestId('create-pr-submit'));

    await waitFor(() => expect(onClose).toHaveBeenCalledTimes(1));
    expect(create).toHaveBeenCalledWith(pane.workspace, 'Add login', 'Supports SSO.', true);
    const toast = getToasts().at(-1);
    expect(toast?.type).toBe('success');
    expect(toast?.message).toBe('PR created: https://github.com/o/r/pull/7');
  });

  it('shows a result error inline and keeps the dialog open', async () => {
    setBindingMock('GitCreatePR', async () => ({
      action: 'pr',
      error: 'gh pr create failed: must push first',
    } as GitActionResult));
    const { onClose, getByTestId, findByRole } = await mount();
    await fireEvent.input(fields().title, { target: { value: 'Add login' } });
    await fireEvent.click(getByTestId('create-pr-submit'));

    const alert = await findByRole('alert');
    expect(alert.textContent).toContain('must push first');
    expect(onClose).not.toHaveBeenCalled();
    expect((getByTestId('create-pr-submit') as HTMLButtonElement).disabled).toBe(false);
    expect(fields().title.value).toBe('Add login');
  });

  it('shows a thrown error inline', async () => {
    setBindingMock('GitCreatePR', async () => {
      throw new Error('glab is not installed');
    });
    const { onClose, getByTestId, findByRole } = await mount(status({ forge: 'gitlab' }));
    await fireEvent.input(fields().title, { target: { value: 'Add login' } });
    await fireEvent.click(getByTestId('create-pr-submit'));

    const alert = await findByRole('alert');
    expect(alert.textContent).toContain('glab is not installed');
    expect(onClose).not.toHaveBeenCalled();
  });

  it('ignores a second submit while the first is in flight', async () => {
    let resolveCreate: (r: GitActionResult) => void = () => {};
    const create = setBindingMock('GitCreatePR', () => new Promise<GitActionResult>((resolve) => {
      resolveCreate = resolve;
    }));
    const { getByTestId } = await mount();
    await fireEvent.input(fields().title, { target: { value: 'Add login' } });
    await fireEvent.click(getByTestId('create-pr-submit'));
    await fireEvent.click(getByTestId('create-pr-submit'));
    expect(create).toHaveBeenCalledTimes(1);
    resolveCreate({ action: 'pr', prUrl: 'https://github.com/o/r/pull/8' });
  });
});

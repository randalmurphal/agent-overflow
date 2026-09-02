import { beforeEach, describe, expect, it, vi } from 'vitest';
import { render, fireEvent, waitFor } from '@testing-library/svelte';
import CommitStep from './CommitStep.svelte';
import { createShipChangesState } from '../../../stores/shipChanges.svelte';
import { setBindingMock } from '../../../../test/mocks/bindings-app';
import type { WorkspaceRef } from '../../../types/git';

const WS: WorkspaceRef = { projectId: 'project-1', workspacePath: '/workspace' };

function makeStateWithChanges() {
  const state = createShipChangesState();
  state.open('thread-1', WS);
  state.setStatus({
    isRepo: true,
    branch: 'feature/x',
    isDefaultBranch: false,
    hasChanges: true,
    fileCount: 3,
    insertions: 42,
    deletions: 7,
    hasUpstream: true,
    aheadCount: 0,
    behindCount: 0,
    hasOriginRemote: true,
    pendingOperation: '',
  });
  return state;
}

function makeStateClean() {
  const state = createShipChangesState();
  state.open('thread-1', WS);
  state.setStatus({
    isRepo: true,
    branch: 'feature/x',
    isDefaultBranch: false,
    hasChanges: false,
    fileCount: 0,
    insertions: 0,
    deletions: 0,
    hasUpstream: true,
    aheadCount: 0,
    behindCount: 0,
    hasOriginRemote: true,
    pendingOperation: '',
  });
  return state;
}

describe('<CommitStep> — Generate commit message', () => {
  beforeEach(() => {
    setBindingMock('GenerateCommitMessage', async () => ({
      subject: 'Add login flow',
      body: 'Supports SSO.',
    }));
  });

  it('renders a Generate button when there are changes to commit', () => {
    const state = makeStateWithChanges();
    const { getByTestId } = render(CommitStep, {
      state, onCommit: vi.fn(), onSkip: vi.fn(),
    });
    expect(getByTestId('ship-changes-generate-message')).toBeInTheDocument();
  });

  it('disables the Generate button when there are no changes', () => {
    const state = makeStateClean();
    const { getByTestId } = render(CommitStep, {
      state, onCommit: vi.fn(), onSkip: vi.fn(),
    });
    const btn = getByTestId('ship-changes-generate-message') as HTMLButtonElement;
    expect(btn.disabled).toBe(true);
  });

  it('populates subject and body on successful generation', async () => {
    const state = makeStateWithChanges();
    const { getByTestId } = render(CommitStep, {
      state, onCommit: vi.fn(), onSkip: vi.fn(),
    });
    await fireEvent.click(getByTestId('ship-changes-generate-message'));
    await waitFor(() => {
      const subject = getByTestId('ship-changes-commit-subject') as HTMLInputElement;
      expect(subject.value).toBe('Add login flow');
    });
    const body = getByTestId('ship-changes-commit-body') as HTMLTextAreaElement;
    expect(body.value).toBe('Supports SSO.');
  });

  it('shows "Generating…" while the request is in flight', async () => {
    let release: () => void = () => {};
    const pending = new Promise<{ subject: string; body: string }>((resolve) => {
      release = () => resolve({ subject: 'done', body: '' });
    });
    setBindingMock('GenerateCommitMessage', async () => pending);
    const state = makeStateWithChanges();
    const { getByTestId } = render(CommitStep, {
      state, onCommit: vi.fn(), onSkip: vi.fn(),
    });
    const btn = getByTestId('ship-changes-generate-message') as HTMLButtonElement;
    void fireEvent.click(btn);
    await waitFor(() => expect(btn.textContent).toMatch(/generating/i));
    release();
    await waitFor(() => expect(btn.textContent).not.toMatch(/generating/i));
  });

  it('re-enables the Generate button after the request completes', async () => {
    const state = makeStateWithChanges();
    const { getByTestId } = render(CommitStep, {
      state, onCommit: vi.fn(), onSkip: vi.fn(),
    });
    const btn = getByTestId('ship-changes-generate-message') as HTMLButtonElement;
    await fireEvent.click(btn);
    await waitFor(() => expect(btn.disabled).toBe(false));
  });

  it('surfaces generation errors via the toast and leaves the fields alone', async () => {
    setBindingMock('GenerateCommitMessage', async () => {
      throw new Error('claude CLI failed');
    });
    const state = makeStateWithChanges();
    state.setCommitSubject('manual subject');
    state.setCommitBody('manual body');
    const { getByTestId } = render(CommitStep, {
      state, onCommit: vi.fn(), onSkip: vi.fn(),
    });
    await fireEvent.click(getByTestId('ship-changes-generate-message'));
    // Wait for the in-flight state to clear so we know the error path ran.
    const btn = getByTestId('ship-changes-generate-message') as HTMLButtonElement;
    await waitFor(() => expect(btn.textContent).not.toMatch(/generating/i));
    // User's hand-typed message must survive the failed request.
    expect((getByTestId('ship-changes-commit-subject') as HTMLInputElement).value).toBe('manual subject');
    expect((getByTestId('ship-changes-commit-body') as HTMLTextAreaElement).value).toBe('manual body');
  });

  it('ignores clicks while already generating (no duplicate calls)', async () => {
    let release: () => void = () => {};
    const pending = new Promise<{ subject: string; body: string }>((resolve) => {
      release = () => resolve({ subject: 's', body: '' });
    });
    const mock = setBindingMock('GenerateCommitMessage', async () => pending);
    const state = makeStateWithChanges();
    const { getByTestId } = render(CommitStep, {
      state, onCommit: vi.fn(), onSkip: vi.fn(),
    });
    const btn = getByTestId('ship-changes-generate-message');
    void fireEvent.click(btn);
    void fireEvent.click(btn);
    void fireEvent.click(btn);
    release();
    await waitFor(() => expect((btn as HTMLButtonElement).textContent).not.toMatch(/generating/i));
    // Only one backend call should have fired, despite three clicks.
    expect(mock).toHaveBeenCalledTimes(1);
  });
});

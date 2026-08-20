import { beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, waitFor } from '@testing-library/svelte';
import type { WorktreeSetupEvent } from '../../types/events';

const GetThreadWorktreeSetup = vi.fn();
const RetryThreadWorktreeSetup = vi.fn();
const addToast = vi.fn();

vi.mock('../../stores/bindings', () => ({
  GetThreadWorktreeSetup: (...args: unknown[]) => GetThreadWorktreeSetup(...args),
  RetryThreadWorktreeSetup: (...args: unknown[]) => RetryThreadWorktreeSetup(...args),
}));
vi.mock('../../stores/toast.svelte', () => ({
  addToast: (...args: unknown[]) => addToast(...args),
}));

const {
  applyWorktreeSetupEvent,
  getWorktreeSetup,
  resetWorktreeSetupForTest,
} = await import('../../stores/worktreeSetup.svelte');
const { default: WorktreeSetupPanel } = await import('./WorktreeSetupPanel.svelte');

const THREAD = 't1';

function started(runId = 'run-1'): WorktreeSetupEvent {
  return {
    phase: 'started',
    threadId: THREAD,
    runId,
    worktreePath: '/wt',
    startedAt: Date.now(),
    steps: [
      { index: 0, kind: 'copy', label: 'Copy files' },
      { index: 1, kind: 'command', label: 'pnpm install', argv: ['pnpm', 'install'] },
    ],
  };
}

function finish(state: string, error = '', runId = 'run-1'): WorktreeSetupEvent {
  return { phase: 'finished', threadId: THREAD, runId, state, error, finishedAt: Date.now() };
}

beforeEach(() => {
  resetWorktreeSetupForTest();
  GetThreadWorktreeSetup.mockReset();
  RetryThreadWorktreeSetup.mockReset();
  addToast.mockReset();
});

describe('<WorktreeSetupPanel>', () => {
  it('renders nothing for a thread with no run', () => {
    const { container } = render(WorktreeSetupPanel, { props: { setupKey: THREAD } });
    expect(container.querySelector('[data-testid="worktree-setup-panel"]')).toBeNull();
    expect(container.querySelector('[data-testid="worktree-setup-bar"]')).toBeNull();
  });

  it('shows every step with its status while running', async () => {
    applyWorktreeSetupEvent(started());
    applyWorktreeSetupEvent({ phase: 'step-started', threadId: THREAD, runId: 'run-1', stepIndex: 0 });
    const { container, getByTestId } = render(WorktreeSetupPanel, { props: { setupKey: THREAD } });

    await waitFor(() => {
      expect(getByTestId('worktree-setup-panel')).toBeTruthy();
    });
    expect(container.textContent).toContain('Setting up worktree');
    const steps = container.querySelectorAll('[data-testid="worktree-setup-step"]');
    expect(steps.length).toBe(2);
    expect(steps[0].getAttribute('data-step-status')).toBe('running');
    expect(steps[1].getAttribute('data-step-status')).toBe('pending');
    // Running runs offer no Retry — there is nothing settled to retry.
    expect(container.querySelector('[data-testid="worktree-setup-retry"]')).toBeNull();
  });

  it('streams output into the panel', async () => {
    applyWorktreeSetupEvent(started());
    const { container } = render(WorktreeSetupPanel, { props: { setupKey: THREAD } });
    applyWorktreeSetupEvent({
      phase: 'output', threadId: THREAD, runId: 'run-1', stepIndex: 1, seq: 1, chunk: 'installing deps\n',
    });
    await waitFor(() => {
      expect(container.textContent).toContain('installing deps');
    });
  });

  // Success is an acknowledgement, not information to act on. The backend has
  // already dropped its record, so the card clears itself.
  it('auto-dismisses a successful run', async () => {
    vi.useFakeTimers();
    try {
      applyWorktreeSetupEvent(started());
      applyWorktreeSetupEvent(finish('succeeded'));
      const { container } = render(WorktreeSetupPanel, { props: { setupKey: THREAD } });
      await vi.advanceTimersByTimeAsync(0);
      expect(container.textContent).toContain('Worktree ready');

      await vi.advanceTimersByTimeAsync(3_000);
      expect(getWorktreeSetup(THREAD)).toBeNull();
    } finally {
      vi.useRealTimers();
    }
  });

  it('keeps a failure up with the failed step and a retry', async () => {
    applyWorktreeSetupEvent(started());
    applyWorktreeSetupEvent({
      phase: 'step-finished', threadId: THREAD, runId: 'run-1', stepIndex: 0, state: 'succeeded',
    });
    applyWorktreeSetupEvent({
      phase: 'step-finished', threadId: THREAD, runId: 'run-1', stepIndex: 1, state: 'failed',
    });
    applyWorktreeSetupEvent(finish('failed', 'command pnpm install failed: exit status 1'));

    const { container, getByTestId } = render(WorktreeSetupPanel, { props: { setupKey: THREAD } });
    await waitFor(() => {
      expect(getByTestId('worktree-setup-panel').getAttribute('data-state')).toBe('failed');
    });
    expect(container.textContent).toContain('Worktree setup failed');
    const steps = container.querySelectorAll('[data-testid="worktree-setup-step"]');
    expect(steps[1].getAttribute('data-step-status')).toBe('failed');
    // The error names the step that failed, not just the raw message.
    expect(getByTestId('worktree-setup-error').textContent).toContain('pnpm install');
    expect(getByTestId('worktree-setup-error').textContent).toContain('exit status 1');
  });

  // Dismiss COLLAPSES — it does not hide. The worktree is genuinely
  // under-provisioned until something fixes it.
  it('collapses a failure to a one-line bar and back', async () => {
    applyWorktreeSetupEvent(started());
    applyWorktreeSetupEvent(finish('failed', 'boom'));
    const { container, getByTestId } = render(WorktreeSetupPanel, { props: { setupKey: THREAD } });
    await waitFor(() => expect(getByTestId('worktree-setup-dismiss')).toBeTruthy());

    await fireEvent.click(getByTestId('worktree-setup-dismiss'));
    await waitFor(() => expect(getByTestId('worktree-setup-bar')).toBeTruthy());
    expect(container.querySelector('[data-testid="worktree-setup-panel"]')).toBeNull();
    expect(getByTestId('worktree-setup-bar').textContent).toContain('Worktree setup failed');
    // Retry stays reachable from the collapsed bar.
    expect(getByTestId('worktree-setup-bar-retry')).toBeTruthy();

    await fireEvent.click(getByTestId('worktree-setup-show'));
    await waitFor(() => expect(getByTestId('worktree-setup-panel')).toBeTruthy());
  });

  it('retries and flips back to running', async () => {
    RetryThreadWorktreeSetup.mockResolvedValue(undefined);
    applyWorktreeSetupEvent(started());
    applyWorktreeSetupEvent(finish('failed', 'boom'));
    const { getByTestId } = render(WorktreeSetupPanel, { props: { setupKey: THREAD } });
    await waitFor(() => expect(getByTestId('worktree-setup-retry')).toBeTruthy());

    await fireEvent.click(getByTestId('worktree-setup-retry'));
    await waitFor(() => {
      expect(getByTestId('worktree-setup-panel').getAttribute('data-state')).toBe('running');
    });
    expect(RetryThreadWorktreeSetup).toHaveBeenCalledWith(THREAD);
  });

  // A rejected retry must leave the affordance that produced it, or the user is
  // stuck looking at a card they can no longer act on.
  it('surfaces a rejected retry and restores the failure', async () => {
    RetryThreadWorktreeSetup.mockRejectedValue(new Error('thread is not working in a worktree'));
    applyWorktreeSetupEvent(started());
    applyWorktreeSetupEvent(finish('failed', 'boom'));
    const { getByTestId } = render(WorktreeSetupPanel, { props: { setupKey: THREAD } });
    await waitFor(() => expect(getByTestId('worktree-setup-retry')).toBeTruthy());

    await fireEvent.click(getByTestId('worktree-setup-retry'));
    await waitFor(() => expect(addToast).toHaveBeenCalled());
    expect(addToast.mock.calls[0][0]).toBe('error');
    await waitFor(() => {
      expect(getByTestId('worktree-setup-panel').getAttribute('data-state')).toBe('failed');
    });
    expect(getByTestId('worktree-setup-retry')).toBeTruthy();
  });

  it('drops the card when the run is cancelled', async () => {
    applyWorktreeSetupEvent(started());
    const { container, getByTestId } = render(WorktreeSetupPanel, { props: { setupKey: THREAD } });
    await waitFor(() => expect(getByTestId('worktree-setup-panel')).toBeTruthy());

    applyWorktreeSetupEvent(finish('cancelled'));
    await waitFor(() => {
      expect(container.querySelector('[data-testid="worktree-setup-panel"]')).toBeNull();
    });
  });
});

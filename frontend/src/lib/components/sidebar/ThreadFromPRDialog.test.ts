import { describe, expect, it, beforeEach } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import { tick } from 'svelte';
import ThreadFromPRDialog from './ThreadFromPRDialog.svelte';
import { createThreadPane } from '../../stores/thread.svelte';
import { loadSettings } from '../../stores/settings.svelte';
import type { Thread } from '../../types/models';
import { setBindingMock } from '../../../test/mocks/bindings-app';

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

async function buildPane() {
  setBindingMock('SwitchThread', async () => {});
  setBindingMock('ListItems', async () => []);
  setBindingMock('ListPayloadMetas', async () => []);
  const pane = createThreadPane();
  return pane;
}

async function flush(n = 6): Promise<void> {
  for (let i = 0; i < n; i += 1) await tick();
}

describe('<ThreadFromPRDialog>', () => {
  beforeEach(async () => {
    setBindingMock('GetSettings', async () => null);
    setBindingMock('GetProviderStatuses', async () => []);
    await loadSettings();
  });

  it('does not render when `open` is false', async () => {
    const pane = await buildPane();
    const { queryByTestId } = render(ThreadFromPRDialog, {
      props: { open: false, pane, onClose: () => {} },
    });
    expect(queryByTestId('thread-from-pr-dialog')).toBeNull();
  });

  it('renders the dialog with URL and model inputs when open', async () => {
    const pane = await buildPane();
    const { getByTestId } = render(ThreadFromPRDialog, {
      props: { open: true, pane, onClose: () => {} },
    });
    await flush();
    expect(getByTestId('thread-from-pr-dialog')).toBeInTheDocument();
    expect(getByTestId('thread-from-pr-url')).toBeInTheDocument();
    expect(getByTestId('thread-from-pr-submit')).toBeInTheDocument();
  });

  it('Create is disabled when the URL is empty', async () => {
    const pane = await buildPane();
    const { getByTestId } = render(ThreadFromPRDialog, {
      props: { open: true, pane, onClose: () => {} },
    });
    await flush();
    const btn = getByTestId('thread-from-pr-submit') as HTMLButtonElement;
    expect(btn.disabled).toBe(true);
  });

  it('shows a parse error for invalid URLs and keeps Create disabled', async () => {
    const pane = await buildPane();
    const { getByTestId, findByTestId } = render(ThreadFromPRDialog, {
      props: { open: true, pane, onClose: () => {} },
    });
    await flush();
    const input = getByTestId('thread-from-pr-url') as HTMLInputElement;
    await fireEvent.input(input, { target: { value: 'not a url' } });
    await flush();

    const err = await findByTestId('thread-from-pr-parse-error');
    expect(err.textContent).toMatch(/Unrecognised/i);
    const btn = getByTestId('thread-from-pr-submit') as HTMLButtonElement;
    expect(btn.disabled).toBe(true);
  });

  it('enables Create and hides the parse error when the URL is valid', async () => {
    const pane = await buildPane();
    const { getByTestId, queryByTestId } = render(ThreadFromPRDialog, {
      props: { open: true, pane, onClose: () => {} },
    });
    await flush();
    const input = getByTestId('thread-from-pr-url') as HTMLInputElement;
    await fireEvent.input(input, { target: { value: 'foo/bar#1' } });
    await flush();

    expect(queryByTestId('thread-from-pr-parse-error')).toBeNull();
    const btn = getByTestId('thread-from-pr-submit') as HTMLButtonElement;
    expect(btn.disabled).toBe(false);
  });

  it('calls CreateThreadFromPR with the parsed owner/repo and number', async () => {
    const pane = await buildPane();
    const created: Thread = {
      id: 'pr-thread',
      title: 'PR #42: demo',
      provider: 'claude',
      workspacePath: '',
      projectPath: '',
      model: 'claude-sonnet-4-6',
      mode: 'chat',
      createdAt: 0,
      updatedAt: 0,
      archived: false,
    };
    const binding = setBindingMock('CreateThreadFromPR', async () => created);
    let closed = 0;
    const { getByTestId } = render(ThreadFromPRDialog, {
      props: {
        open: true,
        pane,
        onClose: () => { closed += 1; },
      },
    });
    await flush();

    const input = getByTestId('thread-from-pr-url') as HTMLInputElement;
    await fireEvent.input(input, { target: { value: 'https://github.com/owner/repo/pull/42' } });
    await flush();

    await fireEvent.click(getByTestId('thread-from-pr-submit'));
    await flush(10);

    expect(binding.mock.calls.length).toBe(1);
    expect(binding.mock.calls[0][0]).toBe('owner/repo');
    expect(binding.mock.calls[0][1]).toBe(42);
    expect(binding.mock.calls[0][2]).toBe('claude');
    expect(binding.mock.calls[0][3]).toBe('');
    // forge id propagates as the 5th positional arg.
    expect(binding.mock.calls[0][4]).toBe('github');
    expect(closed).toBe(1);
  });

  it('passes an explicit model when the optional model field is filled', async () => {
    const pane = await buildPane();
    const created: Thread = {
      id: 'pr-thread-model',
      title: 'PR #12: demo',
      provider: 'claude',
      workspacePath: '',
      projectPath: '',
      model: 'claude-opus-4-7',
      mode: 'chat',
      createdAt: 0,
      updatedAt: 0,
      archived: false,
    };
    const binding = setBindingMock('CreateThreadFromPR', async () => created);
    const { getByTestId } = render(ThreadFromPRDialog, {
      props: { open: true, pane, onClose: () => {} },
    });
    await flush();

    await fireEvent.input(getByTestId('thread-from-pr-url'), {
      target: { value: 'owner/repo#12' },
    });
    await fireEvent.input(getByTestId('thread-from-pr-model'), {
      target: { value: ' claude-opus-4-7 ' },
    });
    await flush();
    await fireEvent.click(getByTestId('thread-from-pr-submit'));
    await flush(10);

    expect(binding.mock.calls.length).toBe(1);
    expect(binding.mock.calls[0][3]).toBe('claude-opus-4-7');
  });

  it('passes forge=gitlab and the full subgroup namespace when a GitLab MR URL is submitted', async () => {
    const pane = await buildPane();
    const created: Thread = {
      id: 't-mr',
      title: 'MR !9 demo',
      provider: 'claude',
      workspacePath: '',
      projectPath: 'pr://gitlab/group/sub/repo',
      model: 'claude-sonnet-4-6',
      mode: 'chat',
      createdAt: 0,
      updatedAt: 0,
      archived: false,
    };
    const binding = setBindingMock('CreateThreadFromPR', async () => created);

    let closed = 0;
    const { getByTestId } = render(ThreadFromPRDialog, {
      props: {
        open: true,
        pane,
        onClose: () => { closed += 1; },
      },
    });
    await flush();

    const input = getByTestId('thread-from-pr-url') as HTMLInputElement;
    await fireEvent.input(input, {
      target: { value: 'https://gitlab.com/group/sub/repo/-/merge_requests/9' },
    });
    await flush();
    await fireEvent.click(getByTestId('thread-from-pr-submit'));
    await flush(10);

    expect(binding.mock.calls.length).toBe(1);
    expect(binding.mock.calls[0][0]).toBe('group/sub/repo');
    expect(binding.mock.calls[0][1]).toBe(9);
    expect(binding.mock.calls[0][4]).toBe('gitlab');
    expect(closed).toBe(1);
  });

  it('surfaces the error message inline when CreateThreadFromPR rejects', async () => {
    const pane = await buildPane();
    setBindingMock('CreateThreadFromPR', async () => {
      throw new Error('gh not installed');
    });
    let closed = 0;
    const { getByTestId, findByTestId } = render(ThreadFromPRDialog, {
      props: {
        open: true,
        pane,
        onClose: () => { closed += 1; },
      },
    });
    await flush();

    const input = getByTestId('thread-from-pr-url') as HTMLInputElement;
    await fireEvent.input(input, { target: { value: 'foo/bar#7' } });
    await flush();
    await fireEvent.click(getByTestId('thread-from-pr-submit'));
    await flush(10);

    const banner = await findByTestId('thread-from-pr-error');
    expect(banner.textContent).toMatch(/gh not installed/);
    expect(closed).toBe(0);
  });

  it('closes when Escape is pressed', async () => {
    const pane = await buildPane();
    let closed = 0;
    const { container } = render(ThreadFromPRDialog, {
      props: {
        open: true,
        pane,
        onClose: () => { closed += 1; },
      },
    });
    await flush();
    // Escape is routed through Modal's backdrop keydown listener. The
    // `data-modal-backdrop` attribute is Modal's stable contract for
    // tests; the previous `thread-from-pr-backdrop` testid went away
    // with the consolidation onto the shared primitive.
    const backdrop = container.querySelector('[data-modal-backdrop]')!;
    await fireEvent.keyDown(backdrop, { key: 'Escape' });
    expect(closed).toBe(1);
  });

  it('submits on Enter when the URL is valid', async () => {
    const pane = await buildPane();
    const created: Thread = {
      id: 'pr-thread',
      title: 'PR #9: sample',
      provider: 'claude',
      workspacePath: '',
      projectPath: '',
      model: 'claude-sonnet-4-6',
      mode: 'chat',
      createdAt: 0,
      updatedAt: 0,
      archived: false,
    };
    const binding = setBindingMock('CreateThreadFromPR', async () => created);
    const { getByTestId } = render(ThreadFromPRDialog, {
      props: {
        open: true,
        pane,
        onClose: () => {},
      },
    });
    await flush();
    const input = getByTestId('thread-from-pr-url') as HTMLInputElement;
    await fireEvent.input(input, { target: { value: 'foo/bar#9' } });
    await flush();
    await fireEvent.keyDown(input, { key: 'Enter' });
    await flush(10);
    expect(binding.mock.calls.length).toBe(1);
    expect(binding.mock.calls[0][1]).toBe(9);
  });

  // Bug C5 regression: the dialog used to navigate + toast + close
  // unconditionally on CreateThreadFromPR success, even if the user had
  // already dismissed the dialog while the RPC was in flight. That yanked
  // the user into a thread they didn't finish confirming. The fix
  // snapshots a generation counter on submit and bails on mismatch.
  it('does not switch panes when the dialog closes before CreateThreadFromPR resolves', async () => {
    const pane = await buildPane();
    const switchSpy = setBindingMock('SwitchThread', async () => {});
    const created: Thread = {
      id: 'late-thread',
      title: 'PR #42: delayed',
      provider: 'claude',
      workspacePath: '',
      projectPath: '',
      model: 'claude-sonnet-4-6',
      mode: 'chat',
      createdAt: 0,
      updatedAt: 0,
      archived: false,
    };
    let resolveCreate!: (value: Thread) => void;
    const createPromise = new Promise<Thread>((r) => { resolveCreate = r; });
    setBindingMock('CreateThreadFromPR', () => createPromise);

    let closed = 0;
    const { getByTestId, rerender } = render(ThreadFromPRDialog, {
      props: {
        open: true,
        pane,
        onClose: () => { closed += 1; },
      },
    });
    await flush();

    const input = getByTestId('thread-from-pr-url') as HTMLInputElement;
    await fireEvent.input(input, { target: { value: 'foo/bar#42' } });
    await flush();
    await fireEvent.click(getByTestId('thread-from-pr-submit'));
    await flush();

    // User dismisses the dialog before the backend answers.
    await rerender({ open: false, pane, onClose: () => { closed += 1; } });
    await flush();

    // Now the backend answers — don't navigate, don't call onClose
    // (dialog is already closed), and don't show a success toast.
    const before = switchSpy.mock.calls.length;
    resolveCreate(created);
    await flush(20);

    // SwitchThread must not have been invoked because the dialog was
    // dismissed; pane.thread is untouched.
    expect(switchSpy.mock.calls.length).toBe(before);
    expect(pane.thread).toBeNull();
  });

  it('still navigates and closes on the happy path', async () => {
    // Paired sanity check for the generation logic above — when the user
    // keeps the dialog open the normal flow must still succeed.
    const pane = await buildPane();
    setBindingMock('SwitchThread', async () => {});
    const created: Thread = {
      id: 'happy',
      title: 'PR #1',
      provider: 'claude',
      workspacePath: '',
      projectPath: '',
      model: 'claude-sonnet-4-6',
      mode: 'chat',
      createdAt: 0,
      updatedAt: 0,
      archived: false,
    };
    setBindingMock('CreateThreadFromPR', async () => created);
    let closed = 0;
    const { getByTestId } = render(ThreadFromPRDialog, {
      props: {
        open: true,
        pane,
        onClose: () => { closed += 1; },
      },
    });
    await flush();
    const input = getByTestId('thread-from-pr-url') as HTMLInputElement;
    await fireEvent.input(input, { target: { value: 'foo/bar#1' } });
    await flush();
    await fireEvent.click(getByTestId('thread-from-pr-submit'));
    await flush(10);
    expect(closed).toBe(1);
    expect(pane.thread?.id).toBe('happy');
  });
});

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
      interactionMode: 'default',
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
    const { getByTestId } = render(ThreadFromPRDialog, {
      props: {
        open: true,
        pane,
        onClose: () => { closed += 1; },
      },
    });
    await flush();
    const dialog = getByTestId('thread-from-pr-backdrop');
    await fireEvent.keyDown(dialog, { key: 'Escape' });
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
      interactionMode: 'default',
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
});

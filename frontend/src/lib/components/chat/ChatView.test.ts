// ChatView structural sanity tests. The old responsive-header behavior
// (inline ModelPicker / BranchToolbar / RuntimeModePicker at wide widths,
// CompactHeaderMenu at narrow widths) is gone — those pickers moved to
// the composer toolbar + below-composer bar in Waves 3a/3b. What's left
// is "does ChatView wire the right children?". This file asserts the
// visible contract that's still meaningful after the rewrite.

import { describe, expect, it, beforeAll } from 'vitest';
import { render } from '@testing-library/svelte';
import { tick } from 'svelte';
import ChatView from './ChatView.svelte';
import { createThreadPane } from '../../stores/thread.svelte';
import type { Thread } from '../../types/models';
import { setBindingMock } from '../../../test/mocks/bindings-app';

beforeAll(() => {
  // Svelte transitions used by children call element.animate; happy-dom
  // doesn't implement it. Keep a minimal shim — the chat directory's
  // tests have relied on this for several waves.
  if (typeof (Element.prototype as unknown as { animate?: unknown }).animate !== 'function') {
    (Element.prototype as unknown as { animate: (...args: unknown[]) => unknown }).animate =
      function fakeAnimate() {
        let onfinish: (() => void) | null = null;
        return {
          finished: Promise.resolve(),
          currentTime: 0,
          playState: 'finished' as const,
          cancel() {},
          finish() { onfinish?.(); },
          play() {},
          pause() {},
          reverse() {},
          addEventListener(type: string, cb: EventListener) {
            if (type === 'finish') onfinish = cb as unknown as () => void;
          },
          removeEventListener() {},
          get onfinish() { return onfinish; },
          set onfinish(cb: (() => void) | null) {
            onfinish = cb;
            if (cb) queueMicrotask(cb);
          },
        };
      };
  }
});

function seedThread(): Thread {
  return {
    id: 'thread-1',
    title: 'Test thread',
    provider: 'claude',
    workspacePath: '/tmp',
    projectPath: '/tmp',
    mode: 'chat',
    model: 'claude-sonnet-4-6',
    createdAt: 0,
    updatedAt: 0,
    archived: false,
  };
}

async function buildPane(): Promise<ReturnType<typeof createThreadPane>> {
  setBindingMock('SwitchThread', async () => {});
  setBindingMock('ListItems', async () => []);
  setBindingMock('ListPayloadMetas', async () => []);
  // GitActionsControl calls GetGitStatus on mount; return "not a repo"
  // so the control renders nothing — we don't need a branch chip.
  setBindingMock('GetGitStatus', async () => ({
    isRepo: false,
    branch: '',
    hasChanges: false,
    hasUpstream: false,
    isDefaultBranch: false,
    aheadCount: 0,
    behindCount: 0,
    openPrUrl: '',
    dirty: false,
    files: [],
  }));
  // BranchPicker calls GitListBranches on mount.
  setBindingMock('GitListBranches', async () => []);
  // Composer fetches slash commands lazily when the user types `/` —
  // not on mount — but the binding mock throws on unexpected calls, so
  // stub it defensively.
  setBindingMock('GetThreadSlashCommands', async () => []);

  const pane = createThreadPane();
  await pane.switchThread(seedThread());
  return pane;
}

describe('<ChatView>', () => {
  it('renders the chat header with title + always-visible controls', async () => {
    const pane = await buildPane();
    const { getByTestId } = render(ChatView, { props: { pane } });
    await tick();

    expect(getByTestId('chat-header')).toBeInTheDocument();
    expect(getByTestId('chat-header-title')).toBeInTheDocument();
    expect(getByTestId('chat-header-provider')).toBeInTheDocument();
    expect(getByTestId('diff-panel-toggle')).toBeInTheDocument();
    expect(getByTestId('plan-sidebar-toggle')).toBeInTheDocument();
  });

  it('renders the below-composer bar', async () => {
    const pane = await buildPane();
    const { getByTestId } = render(ChatView, { props: { pane } });
    await tick();
    expect(getByTestId('below-composer-bar')).toBeInTheDocument();
  });

  it('does not render interaction-mode / runtime-mode / branch pickers in the header', async () => {
    const pane = await buildPane();
    const { queryByTestId } = render(ChatView, { props: { pane } });
    await tick();
    // These IDs belonged to the old header chrome; they must be gone
    // from ChatView entirely (the mode cycle button is the composer
    // toolbar's concern now, and the branch picker lives below the
    // composer).
    expect(queryByTestId('interaction-mode-badge')).toBeNull();
    expect(queryByTestId('runtime-mode-trigger')).toBeNull();
    expect(queryByTestId('chat-header-compact')).toBeNull();
    expect(queryByTestId('compact-header-menu-trigger')).toBeNull();
  });

  it('renders the empty-state when no thread is selected', async () => {
    const pane = createThreadPane();
    const { queryByTestId, getByText } = render(ChatView, { props: { pane } });
    await tick();
    expect(queryByTestId('chat-header')).toBeNull();
    expect(getByText('Select or create a thread')).toBeInTheDocument();
  });
});

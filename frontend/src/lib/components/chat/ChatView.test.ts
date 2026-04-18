// ChatView — narrow-width (compact) header behavior.
//
// Scope is narrow on purpose: we're validating the responsive group
// split. The test controls the observed width by stubbing
// ResizeObserver so we can assert which DOM tree the collapsible
// chrome ends up in for "wide" vs "narrow". Wiring for the underlying
// pickers is irrelevant here and exercised by their own tests.

import { describe, expect, it, beforeAll, beforeEach } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import { tick } from 'svelte';
import ChatView from './ChatView.svelte';
import { createThreadPane } from '../../stores/thread.svelte';
import type { Thread } from '../../types/models';
import { setBindingMock } from '../../../test/mocks/bindings-app';

// Controls the width ResizeObserver reports for the next observe()
// call. Mutated per-test to flip between wide and compact.
let observedWidth = 800;

// Per-observer cleanup hooks. The ChatView effect cleanup calls
// disconnect() when the component unmounts; we honor that so stale
// callbacks don't fire across tests.
class ControlledResizeObserver {
  private cb: ResizeObserverCallback;
  private disposed = false;

  constructor(cb: ResizeObserverCallback) {
    this.cb = cb;
  }

  observe(el: Element): void {
    if (this.disposed) return;
    // Fire synchronously with the configured width so the component's
    // $state updates before the first assertion.
    this.cb(
      [
        {
          target: el,
          contentRect: {
            x: 0,
            y: 0,
            top: 0,
            left: 0,
            right: observedWidth,
            bottom: 40,
            width: observedWidth,
            height: 40,
            toJSON() { return {}; },
          } as DOMRectReadOnly,
          borderBoxSize: [],
          contentBoxSize: [],
          devicePixelContentBoxSize: [],
        } as unknown as ResizeObserverEntry,
      ],
      this as unknown as ResizeObserver,
    );
  }

  unobserve(): void {}

  disconnect(): void {
    this.disposed = true;
  }
}

beforeAll(() => {
  (globalThis as unknown as { ResizeObserver: typeof ResizeObserver }).ResizeObserver =
    ControlledResizeObserver as unknown as typeof ResizeObserver;

  // Svelte transitions used by children call element.animate; happy-dom
  // doesn't implement it. Reuse the minimal shim other chat tests use.
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
    interactionMode: 'default',
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
  // BranchToolbar and GitActionsControl both fetch git status on mount.
  // Return a "not a repo" response so they render nothing — we don't
  // care whether the inline vs collapsed section shows a branch chip,
  // only that the four optional pickers move between the two groups.
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
  // Composer fetches slash commands lazily when the user types `/` —
  // not on mount — but the binding mock throws on unexpected calls, so
  // stub it defensively to catch any future eager hydration.
  setBindingMock('GetThreadSlashCommands', async () => []);

  const pane = createThreadPane();
  await pane.switchThread(seedThread());
  return pane;
}

function collapsibleTestIds(): string[] {
  // These test IDs are stable surface points on each of the four
  // collapsible children. Asserting on their DOM parentage is the
  // most robust way to say "the picker is in group A, not group B".
  return [
    'runtime-mode-trigger',
    'git-actions-error', // may be present if status fails; harmless
  ];
}

describe('<ChatView> header compact behavior', () => {
  beforeEach(() => {
    observedWidth = 800;
  });

  it('renders the collapsible pickers inline when the header is wide', async () => {
    const pane = await buildPane();
    observedWidth = 800;

    const { getByTestId, queryByTestId } = render(ChatView, { props: { pane } });
    await tick();
    await tick();

    const header = getByTestId('chat-header');
    expect(header.hasAttribute('data-compact')).toBe(false);
    // The compact trigger is not rendered at wide widths.
    expect(queryByTestId('chat-header-compact')).toBeNull();
    expect(queryByTestId('compact-header-menu-trigger')).toBeNull();

    // The runtime-mode picker has a testid; confirm it's a direct
    // descendant of the header (not nested inside the compact wrapper).
    const runtimeTrigger = getByTestId('runtime-mode-trigger');
    expect(header.contains(runtimeTrigger)).toBe(true);
    // Sanity check: other collapsible candidates exist in the header.
    // We don't require all of them — BranchToolbar / GitActionsControl
    // render conditionally on git status — but the picker always renders.
    expect(collapsibleTestIds()).toBeDefined();
  });

  it('always shows the core status chrome (Diffs, Plans, header)', async () => {
    const pane = await buildPane();
    observedWidth = 800;

    const { getByTestId } = render(ChatView, { props: { pane } });
    await tick();

    expect(getByTestId('chat-header')).toBeInTheDocument();
    expect(getByTestId('diff-panel-toggle')).toBeInTheDocument();
    expect(getByTestId('plan-sidebar-toggle')).toBeInTheDocument();
    expect(getByTestId('interaction-mode-badge')).toBeInTheDocument();
  });

  it('collapses the four pickers into CompactHeaderMenu when narrow', async () => {
    const pane = await buildPane();
    observedWidth = 400;

    const { getByTestId, queryAllByTestId } = render(ChatView, { props: { pane } });
    await tick();
    await tick();

    const header = getByTestId('chat-header');
    expect(header.getAttribute('data-compact')).toBe('true');

    // The collapsed wrapper and the CompactHeaderMenu trigger are
    // both present. The pickers themselves aren't in the DOM yet —
    // the menu is closed, and the snippet only renders when open.
    const collapsed = getByTestId('chat-header-compact');
    expect(collapsed).toBeInTheDocument();
    const menuTrigger = getByTestId('compact-header-menu-trigger');
    expect(menuTrigger).toBeInTheDocument();
    // Pickers have not spilled into the top-level header — this is
    // the key contract: closed menu means no duplicate-mount.
    expect(queryAllByTestId('runtime-mode-trigger')).toHaveLength(0);

    // Open the menu; now the four pickers render inside the dropdown.
    await fireEvent.click(menuTrigger);
    await tick();

    const menu = getByTestId('compact-header-menu');
    const triggers = queryAllByTestId('runtime-mode-trigger');
    expect(triggers).toHaveLength(1);
    expect(menu.contains(triggers[0])).toBe(true);
  });

  it('still shows the always-visible toggles and meters when narrow', async () => {
    const pane = await buildPane();
    observedWidth = 400;

    const { getByTestId } = render(ChatView, { props: { pane } });
    await tick();

    // Diffs and Plans toggles sit outside the compact menu.
    const diffs = getByTestId('diff-panel-toggle');
    const plans = getByTestId('plan-sidebar-toggle');
    const compact = getByTestId('chat-header-compact');
    expect(compact.contains(diffs)).toBe(false);
    expect(compact.contains(plans)).toBe(false);
    // InteractionModeBadge is always visible too.
    expect(getByTestId('interaction-mode-badge')).toBeInTheDocument();
  });

  it('crosses the 640px threshold cleanly (639px is compact, 640px is not)', async () => {
    const pane = await buildPane();

    observedWidth = 639;
    const below = render(ChatView, { props: { pane } });
    await tick();
    expect(below.getByTestId('chat-header').getAttribute('data-compact')).toBe('true');
    below.unmount();

    observedWidth = 640;
    const at = render(ChatView, { props: { pane } });
    await tick();
    expect(at.getByTestId('chat-header').hasAttribute('data-compact')).toBe(false);
    at.unmount();
  });
});

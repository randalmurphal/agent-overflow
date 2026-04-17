import { describe, expect, it, vi } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import InteractionModeBadge from './InteractionModeBadge.svelte';
import { createThreadPane } from '../../stores/thread.svelte';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import type { Thread } from '../../types/models';

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

function makeThread(overrides: Partial<Thread> = {}): Thread {
  return {
    id: 'thread-1',
    title: 'Example thread',
    provider: 'claude',
    workspacePath: '/workspace',
    projectPath: '/workspace',
    model: 'claude-sonnet-4-6',
    interactionMode: 'default',
    createdAt: 0,
    updatedAt: 0,
    archived: false,
    ...overrides,
  };
}

async function buildPane(thread = makeThread()) {
  setBindingMock('SwitchThread', async () => {});
  setBindingMock('ListItems', async () => []);
  setBindingMock('ListPayloadMetas', async () => []);
  const pane = createThreadPane();
  await pane.switchThread(thread);
  return pane;
}

async function flushMicrotasks(n = 5): Promise<void> {
  for (let i = 0; i < n; i += 1) {
    await Promise.resolve();
  }
}

describe('<InteractionModeBadge>', () => {
  it('renders the current mode label in uppercase', async () => {
    const pane = await buildPane(makeThread({ interactionMode: 'plan' }));
    const { getByTestId } = render(InteractionModeBadge, { props: { pane } });
    const badge = getByTestId('interaction-mode-badge');
    expect(badge.textContent?.trim()).toBe('PLAN');
  });

  it('opens a menu with all three user-selectable modes when clicked', async () => {
    const pane = await buildPane();
    const { getByTestId, findByTestId } = render(InteractionModeBadge, { props: { pane } });
    await fireEvent.click(getByTestId('interaction-mode-badge'));

    expect(await findByTestId('interaction-mode-option-default')).toBeInTheDocument();
    expect(await findByTestId('interaction-mode-option-plan')).toBeInTheDocument();
    expect(await findByTestId('interaction-mode-option-design')).toBeInTheDocument();
  });

  it('marks the current mode with aria-checked="true"', async () => {
    const pane = await buildPane(makeThread({ interactionMode: 'design' }));
    const { getByTestId, findByTestId } = render(InteractionModeBadge, { props: { pane } });
    await fireEvent.click(getByTestId('interaction-mode-badge'));

    const design = await findByTestId('interaction-mode-option-design');
    expect(design.getAttribute('aria-checked')).toBe('true');
    const plan = await findByTestId('interaction-mode-option-plan');
    expect(plan.getAttribute('aria-checked')).toBe('false');
  });

  it('calls SetThreadInteractionMode and updates the pane on selection', async () => {
    const pane = await buildPane();
    const binding = setBindingMock('SetThreadInteractionMode', async (threadId: string, mode: string) => ({
      ...(pane.thread as Thread),
      interactionMode: mode,
    }));

    const { getByTestId, findByTestId } = render(InteractionModeBadge, { props: { pane } });
    await fireEvent.click(getByTestId('interaction-mode-badge'));
    const planOpt = await findByTestId('interaction-mode-option-plan');
    await fireEvent.click(planOpt);

    await flushMicrotasks();
    expect(binding.mock.calls[0]).toEqual(['thread-1', 'plan']);
    expect(pane.thread?.interactionMode).toBe('plan');
  });

  it('does not call the binding when user picks the already-active mode', async () => {
    const pane = await buildPane(makeThread({ interactionMode: 'plan' }));
    const binding = setBindingMock('SetThreadInteractionMode', async () => pane.thread as Thread);

    const { getByTestId, findByTestId } = render(InteractionModeBadge, { props: { pane } });
    await fireEvent.click(getByTestId('interaction-mode-badge'));
    const planOpt = await findByTestId('interaction-mode-option-plan');
    await fireEvent.click(planOpt);

    await flushMicrotasks();
    expect(binding.mock.calls.length).toBe(0);
  });

  it('surfaces errors on the pane when the binding rejects', async () => {
    const pane = await buildPane();
    setBindingMock('SetThreadInteractionMode', async () => {
      throw new Error('invalid interaction mode');
    });
    const consoleErr = vi.spyOn(console, 'error').mockImplementation(() => {});

    const { getByTestId, findByTestId } = render(InteractionModeBadge, { props: { pane } });
    await fireEvent.click(getByTestId('interaction-mode-badge'));
    const designOpt = await findByTestId('interaction-mode-option-design');
    await fireEvent.click(designOpt);

    await flushMicrotasks(8);
    expect(pane.error).toMatch(/invalid interaction mode/i);
    consoleErr.mockRestore();
  });

  it('renders the Discussion label for discussion threads but omits discussion from the picker', async () => {
    const pane = await buildPane(makeThread({ interactionMode: 'discussion' }));
    const { getByTestId, queryByTestId, findByTestId } = render(InteractionModeBadge, { props: { pane } });
    expect(getByTestId('interaction-mode-badge').textContent?.trim()).toBe('DISCUSSION');
    await fireEvent.click(getByTestId('interaction-mode-badge'));
    await findByTestId('interaction-mode-option-default');
    expect(queryByTestId('interaction-mode-option-discussion')).toBeNull();
  });

  it('closes the menu on Escape key', async () => {
    const pane = await buildPane();
    const { getByTestId, findByTestId } = render(InteractionModeBadge, { props: { pane } });
    const trigger = getByTestId('interaction-mode-badge');
    await fireEvent.click(trigger);

    const menu = await findByTestId('interaction-mode-option-default');
    expect(menu).toBeInTheDocument();
    expect(trigger.getAttribute('aria-expanded')).toBe('true');

    await fireEvent.keyDown(menu, { key: 'Escape' });
    await flushMicrotasks();
    // aria-expanded is the robust state-change signal; the transition
    // may briefly keep the element in the DOM.
    expect(trigger.getAttribute('aria-expanded')).toBe('false');
  });

  it('closes the menu after a successful switch', async () => {
    const pane = await buildPane();
    setBindingMock('SetThreadInteractionMode', async (threadId: string, mode: string) => ({
      ...(pane.thread as Thread),
      interactionMode: mode,
    }));
    const { getByTestId, findByTestId } = render(InteractionModeBadge, { props: { pane } });
    const trigger = getByTestId('interaction-mode-badge');
    await fireEvent.click(trigger);
    const designOpt = await findByTestId('interaction-mode-option-design');
    await fireEvent.click(designOpt);
    await flushMicrotasks();
    expect(trigger.getAttribute('aria-expanded')).toBe('false');
  });

  it('arrow keys move focus within the menu', async () => {
    const pane = await buildPane();
    const { getByTestId, findByTestId } = render(InteractionModeBadge, { props: { pane } });
    await fireEvent.click(getByTestId('interaction-mode-badge'));

    const def = await findByTestId('interaction-mode-option-default');
    await flushMicrotasks();
    // First item receives focus automatically.
    expect(document.activeElement).toBe(def);

    await fireEvent.keyDown(def, { key: 'ArrowDown' });
    expect(document.activeElement).toBe(getByTestId('interaction-mode-option-plan'));
  });
});

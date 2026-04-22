import { beforeEach, describe, expect, it } from 'vitest';
import { fireEvent, render, waitFor } from '@testing-library/svelte';

import ModelProviderMenu from './ModelProviderMenu.svelte';
import { createThreadPane } from '../../../stores/thread.svelte';
import type { Thread } from '../../../types/models';
import {
  getBindingMock,
  resetBindingMocks,
  setBindingMock,
} from '../../../../test/mocks/bindings-app';

function makeThread(overrides: Partial<Thread> = {}): Thread {
  return {
    id: 'thread-1',
    title: 'Test',
    provider: 'claude',
    workspacePath: '/tmp',
    projectPath: '/tmp',
    mode: 'chat',
    model: 'claude-sonnet-4-6',
    createdAt: 0,
    updatedAt: 0,
    archived: false,
    ...overrides,
  };
}

async function buildPane(thread: Thread) {
  setBindingMock('SwitchThread', async () => {});
  setBindingMock('ListItems', async () => []);
  setBindingMock('ListPayloadMetas', async () => []);
  const pane = createThreadPane();
  await pane.switchThread(thread);
  return pane;
}

describe('<ModelProviderMenu>', () => {
  beforeEach(() => {
    resetBindingMocks();
    setBindingMock('ListDiscussions', async () => []);
  });

  it('renders the active model slug on the trigger (brand is the glyph; no provider word)', async () => {
    const pane = await buildPane(makeThread({ provider: 'claude', model: 'claude-haiku-4-6' }));
    const { getByTestId } = render(ModelProviderMenu, { props: { pane } });
    const trigger = getByTestId('composer-model-menu-trigger');
    // Provider identification lives in the brand glyph (lucide-claude /
    // lucide-openai); the label is the model slug only.
    expect(trigger.textContent ?? '').toMatch(/claude-haiku-4-6/);
    expect(trigger.textContent ?? '').not.toMatch(/\bClaude\b/);
    expect(trigger.textContent ?? '').not.toMatch(/\bCodex\b/);
  });

  // Per-picker provider brand glyph: Claude uses the Anthropic mark
  // painted in Anthropic's coral `#d97757`, Codex uses the OpenAI
  // rosette in the muted foreground. Both match t3-code's
  // providerIconClassName convention in
  // apps/web/src/components/chat/ProviderModelPicker.tsx.
  it('renders the Claude brand mark when provider is Claude', async () => {
    const pane = await buildPane(makeThread({ provider: 'claude' }));
    const { getByTestId } = render(ModelProviderMenu, { props: { pane } });
    const trigger = getByTestId('composer-model-menu-trigger');
    expect(trigger.getAttribute('data-provider')).toBe('claude');
    expect(trigger.querySelector('svg.lucide-claude')).not.toBeNull();
    expect(trigger.querySelector('svg.lucide-openai')).toBeNull();
  });

  it('renders the OpenAI brand mark when provider is Codex', async () => {
    const pane = await buildPane(makeThread({ provider: 'codex', model: 'gpt-5' }));
    const { getByTestId } = render(ModelProviderMenu, { props: { pane } });
    const trigger = getByTestId('composer-model-menu-trigger');
    expect(trigger.getAttribute('data-provider')).toBe('codex');
    expect(trigger.querySelector('svg.lucide-openai')).not.toBeNull();
    expect(trigger.querySelector('svg.lucide-claude')).toBeNull();
  });

  it('Claude mark is painted in the Anthropic coral (#d97757)', async () => {
    const pane = await buildPane(makeThread({ provider: 'claude' }));
    const { getByTestId } = render(ModelProviderMenu, { props: { pane } });
    const svg = getByTestId('composer-model-menu-trigger').querySelector('svg.lucide-claude')!;
    expect(svg.getAttribute('class')).toContain('text-[#d97757]');
  });

  it('OpenAI mark inherits the trigger foreground (no bespoke tint)', async () => {
    const pane = await buildPane(makeThread({ provider: 'codex', model: 'gpt-5' }));
    const { getByTestId } = render(ModelProviderMenu, { props: { pane } });
    const svg = getByTestId('composer-model-menu-trigger').querySelector('svg.lucide-openai')!;
    // Must NOT carry the Anthropic coral — it should read as one piece
    // with the label's muted foreground instead.
    expect(svg.getAttribute('class') ?? '').not.toContain('text-[#d97757]');
  });

  it('warms the active provider cache on open', async () => {
    const pane = await buildPane(makeThread({ provider: 'claude' }));
    const modelsMock = setBindingMock('GetModelsForProvider', async () => [
      { slug: 'claude-opus-4-5', name: 'Claude Opus 4.5', provider: 'claude', capabilities: [] },
    ]);
    const { getByTestId } = render(ModelProviderMenu, { props: { pane } });
    await fireEvent.click(getByTestId('composer-model-menu-trigger'));
    await waitFor(() => {
      expect(modelsMock).toHaveBeenCalled();
    });
    expect(modelsMock.mock.calls.some((c) => c[0] === 'claude')).toBe(true);
  });

  it('calls UpdateThreadProvider + UpdateThreadModel when switching providers', async () => {
    const pane = await buildPane(
      makeThread({ provider: 'claude', model: 'claude-sonnet-4-6' }),
    );
    setBindingMock('GetModelsForProvider', async (provider: unknown) => {
      if (provider === 'codex') {
        return [{ slug: 'gpt-5.4', name: 'GPT 5.4', provider: 'codex', capabilities: [] }];
      }
      return [];
    });
    const providerUpdate = makeThread({ provider: 'codex', model: 'claude-sonnet-4-6' });
    const modelUpdate = makeThread({ provider: 'codex', model: 'gpt-5.4' });
    setBindingMock('UpdateThreadProvider', async () => providerUpdate);
    setBindingMock('UpdateThreadModel', async () => modelUpdate);

    const { getByTestId, findByRole } = render(ModelProviderMenu, { props: { pane } });
    await fireEvent.click(getByTestId('composer-model-menu-trigger'));

    // Hover the Codex submenu to trigger its load.
    const codexRow = await findByRole('menuitem', { name: /Codex/i });
    await fireEvent.click(codexRow);

    const gptOption = await findByRole('menuitem', { name: /GPT 5.4/i });
    await fireEvent.click(gptOption);
    await Promise.resolve();
    await Promise.resolve();

    expect(getBindingMock('UpdateThreadProvider')!.mock.calls[0]).toEqual([
      'thread-1',
      'codex',
    ]);
    expect(getBindingMock('UpdateThreadModel')!.mock.calls[0]).toEqual([
      'thread-1',
      'gpt-5.4',
    ]);
  });

  // Regression: Escape inside a nested submenu must close ONLY the
  // submenu, not the whole stack. After the portal-to-body fix every
  // open Popover attaches its own document-level keydown handler, and
  // stopPropagation does NOT stop sibling listeners on the same
  // element — so both parent + child handlers used to fire at once and
  // collapse the entire stack on a single Escape. Fix: the parent
  // checks `hasOpenDescendantPopover` before handling and skips when
  // a deeper popover is live.
  it('Escape inside a submenu closes only the submenu (parent stays open)', async () => {
    const pane = await buildPane(makeThread({ provider: 'claude' }));
    setBindingMock('GetModelsForProvider', async (provider: unknown) => {
      if (provider === 'codex') {
        return [{ slug: 'gpt-5.4', name: 'GPT 5.4', provider: 'codex', capabilities: [] }];
      }
      return [];
    });

    const { getByTestId, findByRole, queryByRole } = render(ModelProviderMenu, {
      props: { pane },
    });
    await fireEvent.click(getByTestId('composer-model-menu-trigger'));

    // Open the Codex submenu.
    const codexRow = await findByRole('menuitem', { name: /Codex/i });
    await fireEvent.click(codexRow);
    // Wait for the submenu's model row to mount.
    await findByRole('menuitem', { name: /GPT 5.4/i });

    // Parent menu + submenu are both open here. Before the fix, this
    // Escape press would collapse the whole stack.
    await fireEvent.keyDown(document, { key: 'Escape' });
    await Promise.resolve();

    // Submenu should be gone — no GPT 5.4 row anymore.
    expect(queryByRole('menuitem', { name: /GPT 5.4/i })).toBeNull();
    // Parent menu should still be mounted — the Codex row is still
    // reachable. If the fix regressed, this would also disappear.
    expect(queryByRole('menuitem', { name: /Codex/i })).not.toBeNull();
  });

  // Regression: real browsers fire `mousedown` BEFORE `click`. Popover
  // listens on document-level mousedown to detect outside clicks — so
  // when the user clicks a model in the nested submenu, mousedown
  // fires first and the parent popover's handler runs. After portal-
  // to-body both popovers are siblings under <body>, so the parent
  // can't see the child via DOM-contains; it used to treat the click
  // as "outside", close itself, unmount the submenu, and leave the
  // click event with nowhere to land. Fire the full mousedown+click
  // sequence here so the regression can't hide behind the single-
  // event shortcut fireEvent.click takes.
  it('selecting a submenu model works when mousedown precedes click (real browser sequence)', async () => {
    const pane = await buildPane(
      makeThread({ provider: 'claude', model: 'claude-sonnet-4-6' }),
    );
    setBindingMock('GetModelsForProvider', async (provider: unknown) => {
      if (provider === 'codex') {
        return [{ slug: 'gpt-5.4', name: 'GPT 5.4', provider: 'codex', capabilities: [] }];
      }
      return [];
    });
    setBindingMock(
      'UpdateThreadProvider',
      async () => makeThread({ provider: 'codex', model: 'claude-sonnet-4-6' }),
    );
    setBindingMock(
      'UpdateThreadModel',
      async () => makeThread({ provider: 'codex', model: 'gpt-5.4' }),
    );

    const { getByTestId, findByRole } = render(ModelProviderMenu, { props: { pane } });

    const trigger = getByTestId('composer-model-menu-trigger');
    await fireEvent.mouseDown(trigger);
    await fireEvent.click(trigger);

    const codexRow = await findByRole('menuitem', { name: /Codex/i });
    await fireEvent.mouseDown(codexRow);
    await fireEvent.click(codexRow);

    const gptOption = await findByRole('menuitem', { name: /GPT 5.4/i });
    // Mousedown on the deep menu item is the critical step. If the
    // parent popover's outside-mousedown fires and closes prematurely,
    // the Menu's microtask-scheduled setFocus tears down, and the
    // later click never reaches onSelect.
    await fireEvent.mouseDown(gptOption);
    await fireEvent.click(gptOption);
    await Promise.resolve();
    await Promise.resolve();

    expect(getBindingMock('UpdateThreadProvider')!.mock.calls.length).toBe(1);
    expect(getBindingMock('UpdateThreadModel')!.mock.calls[0]).toEqual([
      'thread-1',
      'gpt-5.4',
    ]);
  });
});

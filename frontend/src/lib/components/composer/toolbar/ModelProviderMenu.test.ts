import { beforeEach, describe, expect, it } from 'vitest';
import { fireEvent, render, waitFor } from '@testing-library/svelte';

import ModelProviderMenu from './ModelProviderMenu.svelte';
import { loadSettings, resetSettingsForTest } from '../../../stores/settings.svelte';
import type { Item, Thread } from '../../../types/models';
import type { DiscussionDefinition } from '../../../types/discussion';
import {
  getBindingMock,
  resetBindingMocks,
  setBindingMock,
} from '../../../../test/mocks/bindings-app';
import { makeSettings } from '../../../../test/helpers/settings';
import {
  buildPane as buildRegisteredPane,
  makeItem as makeBaseItem,
  makeThread as makeBaseThread,
} from '../../../../test/helpers/chat';

function makeThread(overrides: Partial<Thread> = {}): Thread {
  return makeBaseThread({
    workspacePath: '/tmp',
    projectPath: '/tmp',
    ...overrides,
  });
}

function makeItem(overrides: Partial<Item> = {}): Item {
  return makeBaseItem({
    turnIndex: 1,
    kind: 'user_text',
    role: 'user',
    ...overrides,
  });
}

async function buildPane(thread: Thread, items: Item[] = []) {
  return buildRegisteredPane(thread, items);
}

// Minimal stub; only id/name/scope are read by the menu + submenu under
// test. Mirrors the fixture in DiscussionsSubmenu.test.ts.
const architects: DiscussionDefinition = {
  id: 'architects',
  name: 'Architects',
  scope: 'global',
  projectId: undefined,
  description: 'Architecture review crew',
  participants: [],
  settings: {} as DiscussionDefinition['settings'],
  createdAt: 0,
  updatedAt: 0,
};

describe('<ModelProviderMenu>', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetSettingsForTest();
    setBindingMock('ListDiscussions', async () => []);
    setBindingMock('ListDiscussionsForThread', async () => []);
    setBindingMock('ListChatBarFavorites', async () => []);
    setBindingMock('SetChatBarFavorite', async () => []);
  });

  it('renders the active Claude model as a provider-free display label', async () => {
    const pane = await buildPane(makeThread({ provider: 'claude', model: 'claude-haiku-4-6' }));
    const { getByTestId } = render(ModelProviderMenu, { props: { pane } });
    const trigger = getByTestId('composer-model-menu-trigger');
    expect(trigger.textContent ?? '').toMatch(/\bHaiku 4\.6\b/);
    expect(trigger.textContent ?? '').not.toMatch(/claude-haiku-4-6/);
    expect(trigger.textContent ?? '').not.toMatch(/\bClaude\b/);
    expect(trigger.textContent ?? '').not.toMatch(/\bCodex\b/);
  });

  it('shows the effective fallback model and retries the requested model when reselected', async () => {
    const pane = await buildPane(makeThread({ provider: 'claude', model: 'claude-fable-5' }));
    pane.setEffectiveModel('claude-opus-4-8');
    setBindingMock('GetModelsForProvider', async (provider: unknown) => {
      if (provider !== 'claude') return [];
      return [
        { slug: 'claude-fable-5', name: 'Fable 5', provider: 'claude', capabilities: [] },
        { slug: 'claude-opus-4-8', name: 'Opus 4.8', provider: 'claude', capabilities: [] },
      ];
    });
    const reconnect = setBindingMock('ReconnectSession', async () => {});

    const { getByTestId, findByRole } = render(ModelProviderMenu, { props: { pane } });
    expect(getByTestId('composer-model-menu-trigger').textContent).toContain('Opus 4.8');

    await fireEvent.click(getByTestId('composer-model-menu-trigger'));
    await fireEvent.click(await findByRole('menuitem', { name: /^Claude$/ }));
    await fireEvent.click(await findByRole('menuitem', { name: /Fable 5/i }));

    await waitFor(() => expect(reconnect).toHaveBeenCalledWith('thread-1'));
    expect(pane.thread?.model).toBe('claude-fable-5');
    expect(pane.activeModel).toBe('claude-fable-5');
    expect(getBindingMock('UpdateThreadModelSelection')?.mock.calls.length ?? 0).toBe(0);
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
      { slug: 'claude-opus-4-5', name: 'Opus 4.5', provider: 'claude', capabilities: [] },
    ]);
    const { getByTestId } = render(ModelProviderMenu, { props: { pane } });
    await fireEvent.click(getByTestId('composer-model-menu-trigger'));
    await waitFor(() => {
      expect(modelsMock).toHaveBeenCalled();
    });
    expect(modelsMock.mock.calls.some((c) => c[0] === 'claude')).toBe(true);
  });

  it('renders DB-backed favorites above provider sections with normalized provider icons', async () => {
    const pane = await buildPane(makeThread({ provider: 'claude', model: 'claude-opus-4-7' }));
    setBindingMock('GetModelsForProvider', async () => []);
    // A discussion favorite is only visible once at least one discussion
    // definition exists — see ensureDiscussions/showDiscussions.
    setBindingMock('ListDiscussionsForThread', async () => [architects]);
    setBindingMock('ListChatBarFavorites', async () => [
      {
        kind: 'model',
        provider: 'claude',
        value: 'claude-opus-4-7',
        label: 'Claude Opus 4.7',
        createdAt: 1,
      },
      {
        kind: 'discussion',
        provider: '',
        value: 'architects',
        label: 'Architects',
        createdAt: 2,
      },
    ]);

    const { getByTestId, findByRole } = render(ModelProviderMenu, { props: { pane } });
    await fireEvent.click(getByTestId('composer-model-menu-trigger'));

    const favorite = await findByRole('menuitem', { name: /Opus 4.7/i });
    expect(favorite.querySelector('svg.lucide-claude')).not.toBeNull();
    expect(favorite.textContent ?? '').not.toMatch(/\bClaude\b/);
    await findByRole('menuitem', { name: /Architects/i });
  });

  it('filters favorites whose model is hidden in settings (star survives for re-show)', async () => {
    const pane = await buildPane(makeThread({ provider: 'claude', model: 'claude-sonnet-4-6' }));
    setBindingMock('GetModelsForProvider', async () => []);
    setBindingMock('GetSettings', async () =>
      makeSettings({ claudeHiddenModels: ['claude-opus-4-7'] }));
    await loadSettings();
    setBindingMock('ListChatBarFavorites', async () => [
      { kind: 'model', provider: 'claude', value: 'claude-opus-4-7', label: 'Claude Opus 4.7', createdAt: 1 },
      { kind: 'model', provider: 'claude', value: 'claude-opus-4-8', label: 'Claude Opus 4.8', createdAt: 2 },
    ]);

    const { getByTestId, findByRole, queryByRole } = render(ModelProviderMenu, { props: { pane } });
    await fireEvent.click(getByTestId('composer-model-menu-trigger'));

    await findByRole('menuitem', { name: /Opus 4\.8/i });
    expect(queryByRole('menuitem', { name: /Opus 4\.7/i })).toBeNull();

    // Re-showing the model in settings brings the star back — the
    // favorite row was filtered, never deleted.
    setBindingMock('GetSettings', async () => makeSettings({ claudeHiddenModels: [] }));
    await loadSettings();
    await findByRole('menuitem', { name: /Opus 4\.7/i });
  });

  it('claude-tui submenu shares the claude hide-list', async () => {
    const pane = await buildPane(makeThread({ provider: 'claude', model: 'claude-sonnet-4-6' }));
    setBindingMock('GetSettings', async () =>
      makeSettings({ claudeHiddenModels: ['claude-opus-4-7'] }));
    await loadSettings();
    setBindingMock('GetModelsForProvider', async () => [
      { slug: 'claude-opus-4-8', name: 'Claude Opus 4.8', provider: 'claude-tui', capabilities: [] },
      { slug: 'claude-opus-4-7', name: 'Claude Opus 4.7', provider: 'claude-tui', capabilities: [] },
    ]);

    const { getByTestId, findByRole, queryByRole } = render(ModelProviderMenu, { props: { pane } });
    await fireEvent.click(getByTestId('composer-model-menu-trigger'));
    const tuiRow = await findByRole('menuitem', { name: /^Claude TUI$/ });
    await fireEvent.click(tuiRow);

    await findByRole('menuitem', { name: /Opus 4\.8/i });
    expect(queryByRole('menuitem', { name: /Opus 4\.7/i })).toBeNull();
  });

  it('drops hidden models from the provider submenu but keeps the active model row', async () => {
    const pane = await buildPane(makeThread({ provider: 'claude', model: 'claude-opus-4-5' }));
    setBindingMock('GetSettings', async () =>
      makeSettings({ claudeHiddenModels: ['claude-opus-4-5', 'claude-opus-4-7'] }));
    await loadSettings();
    setBindingMock('GetModelsForProvider', async () => [
      { slug: 'claude-opus-4-8', name: 'Claude Opus 4.8', provider: 'claude', capabilities: [] },
      { slug: 'claude-opus-4-7', name: 'Claude Opus 4.7', provider: 'claude', capabilities: [] },
      { slug: 'claude-opus-4-5', name: 'Claude Opus 4.5', provider: 'claude', capabilities: [] },
    ]);

    const { getByTestId, findByRole, queryByRole } = render(ModelProviderMenu, { props: { pane } });
    await fireEvent.click(getByTestId('composer-model-menu-trigger'));
    const claudeRow = await findByRole('menuitem', { name: /^Claude$/ });
    await fireEvent.click(claudeRow);

    await findByRole('menuitem', { name: /Opus 4\.8/i });
    // Active model stays listed even though it's hidden — the picker
    // must never show "nothing selected".
    await findByRole('menuitem', { name: /Opus 4\.5/i });
    // Hidden non-active model is gone.
    expect(queryByRole('menuitem', { name: /Opus 4\.7/i })).toBeNull();
  });

  it('renders provider model rows with the favorite star before the label', async () => {
    const pane = await buildPane(makeThread({ provider: 'claude', model: 'claude-sonnet-4-6' }));
    setBindingMock('GetModelsForProvider', async () => [
      { slug: 'claude-opus-4-7', name: 'Claude Opus 4.7', provider: 'claude', capabilities: [] },
    ]);
    setBindingMock('ListChatBarFavorites', async () => [
      {
        kind: 'model',
        provider: 'claude',
        value: 'claude-opus-4-7',
        label: 'Claude Opus 4.7',
        createdAt: 1,
      },
    ]);

    const { getByTestId, findByRole } = render(ModelProviderMenu, { props: { pane } });
    await fireEvent.click(getByTestId('composer-model-menu-trigger'));
    // Anchored so it targets the "Claude" submenu, not "Claude TUI".
    const claudeRow = await findByRole('menuitem', { name: /^Claude$/ });
    await fireEvent.click(claudeRow);

    await waitFor(() => {
      expect(document.querySelector(
        '[role="menuitem"] button[aria-label="Remove Opus 4.7 from favorites"]',
      )).not.toBeNull();
    });
    const option = document.querySelector(
      '[role="menuitem"] button[aria-label="Remove Opus 4.7 from favorites"]',
    )!.closest('[role="menuitem"]')!;
    const star = option.querySelector('button[aria-label="Remove Opus 4.7 from favorites"]');
    expect(star).not.toBeNull();
    expect(option.textContent?.trim().startsWith('Opus 4.7')).toBe(true);
    expect(star!.compareDocumentPosition(option.querySelector('span.min-w-0')!)).toBe(
      Node.DOCUMENT_POSITION_FOLLOWING,
    );
  });

  it('calls UpdateThreadModelSelection when switching providers', async () => {
    const pane = await buildPane(
      makeThread({ provider: 'claude', model: 'claude-sonnet-4-6' }),
    );
    setBindingMock('GetModelsForProvider', async (provider: unknown) => {
      if (provider === 'codex') {
        return [{ slug: 'gpt-5.4', name: 'GPT 5.4', provider: 'codex', capabilities: [] }];
      }
      return [];
    });
    const modelUpdate = makeThread({ provider: 'codex', model: 'gpt-5.4' });
    setBindingMock('UpdateThreadModelSelection', async () => modelUpdate);

    const { getByTestId, findByRole } = render(ModelProviderMenu, { props: { pane } });
    await fireEvent.click(getByTestId('composer-model-menu-trigger'));

    // Hover the Codex submenu to trigger its load.
    const codexRow = await findByRole('menuitem', { name: /Codex/i });
    await fireEvent.click(codexRow);

    const gptOption = await findByRole('menuitem', { name: /GPT 5.4/i });
    await fireEvent.click(gptOption);
    await Promise.resolve();
    await Promise.resolve();

    expect(getBindingMock('UpdateThreadModelSelection')!.mock.calls[0]).toEqual([
      'thread-1',
      'codex',
      'gpt-5.4',
    ]);
    expect(getBindingMock('UpdateThreadProvider')?.mock.calls.length ?? 0).toBe(0);
    expect(getBindingMock('UpdateThreadModel')?.mock.calls.length ?? 0).toBe(0);
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
      'UpdateThreadModelSelection',
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

    expect(getBindingMock('UpdateThreadModelSelection')!.mock.calls[0]).toEqual([
      'thread-1',
      'codex',
      'gpt-5.4',
    ]);
    expect(getBindingMock('UpdateThreadProvider')?.mock.calls.length ?? 0).toBe(0);
    expect(getBindingMock('UpdateThreadModel')?.mock.calls.length ?? 0).toBe(0);
  });

  // Provider lock: once a thread has been used (any item persisted), the
  // thread has picked a lane. Locked chats expose only their own provider's
  // models — never the other provider, and never the Discussions entry (a
  // mid-conversation promotion to discussion would orphan the prior chat
  // messages behind DiscussionView, which is why ensureDiscussionCanStart
  // rejects it server-side). Unlocked (empty) threads still see all three.
  it('on a locked chat, shows only the active provider — no other provider, no Discussions', async () => {
    const pane = await buildPane(
      makeThread({ provider: 'claude', model: 'claude-sonnet-4-6' }),
      [makeItem()],
    );
    setBindingMock('GetModelsForProvider', async () => []);

    const { getByTestId, queryByRole, findByRole } = render(ModelProviderMenu, {
      props: { pane },
    });
    await fireEvent.click(getByTestId('composer-model-menu-trigger'));

    // Active provider's submenu stays reachable so in-provider model swaps
    // (sonnet ↔ opus) still work.
    await findByRole('menuitem', { name: /Claude/i });
    // Neither the other provider nor the discussion entry are offered.
    expect(queryByRole('menuitem', { name: /Codex/i })).toBeNull();
    expect(queryByRole('menuitem', { name: /Discussions/i })).toBeNull();
  });

  it('on a fresh (empty) thread, shows every provider AND Discussions', async () => {
    const pane = await buildPane(
      makeThread({ provider: 'claude', model: 'claude-sonnet-4-6' }),
    );
    setBindingMock('GetModelsForProvider', async () => []);
    setBindingMock('ListDiscussionsForThread', async () => [architects]);

    const { getByTestId, findByRole } = render(ModelProviderMenu, {
      props: { pane },
    });
    await fireEvent.click(getByTestId('composer-model-menu-trigger'));

    // All three providers are offered on a fresh thread — Claude, the
    // interactive Claude TUI, and Codex — plus Discussions. The Claude match is
    // anchored so it doesn't also pick up "Claude TUI".
    await findByRole('menuitem', { name: /^Claude$/ });
    await findByRole('menuitem', { name: 'Claude TUI' });
    await findByRole('menuitem', { name: /Codex/i });
    await findByRole('menuitem', { name: /Discussions/i });
  });

  // Headline regression: the Discussions entry must not render at all
  // when zero discussion definitions exist — opening it just showed
  // "No discussions defined" with no way to act on it. Before the fix,
  // showDiscussions never considered definition existence, so this
  // failed (the entry rendered unconditionally).
  it('hides the Discussions entry when zero discussion definitions exist', async () => {
    const pane = await buildPane(
      makeThread({ provider: 'claude', model: 'claude-sonnet-4-6' }),
    );
    setBindingMock('GetModelsForProvider', async () => []);
    setBindingMock('ListDiscussionsForThread', async () => []);

    const { getByTestId, findByRole, queryByRole } = render(ModelProviderMenu, {
      props: { pane },
    });
    await fireEvent.click(getByTestId('composer-model-menu-trigger'));

    // Wait for the menu to settle (Claude submenu row present) before
    // asserting absence, so we're not just observing a not-yet-rendered menu.
    await findByRole('menuitem', { name: /^Claude$/ });
    await waitFor(() => {
      expect(queryByRole('menuitem', { name: /Discussions/i })).toBeNull();
    });
  });

  it('shows the Discussions entry when at least one discussion definition exists', async () => {
    const pane = await buildPane(
      makeThread({ provider: 'claude', model: 'claude-sonnet-4-6' }),
    );
    setBindingMock('GetModelsForProvider', async () => []);
    setBindingMock('ListDiscussionsForThread', async () => [architects]);

    const { getByTestId, findByRole } = render(ModelProviderMenu, {
      props: { pane },
    });
    await fireEvent.click(getByTestId('composer-model-menu-trigger'));

    await findByRole('menuitem', { name: /Discussions/i });
  });

  it('a null ListDiscussionsForThread result also hides the entry', async () => {
    const pane = await buildPane(
      makeThread({ provider: 'claude', model: 'claude-sonnet-4-6' }),
    );
    setBindingMock('GetModelsForProvider', async () => []);
    setBindingMock('ListDiscussionsForThread', async () => null);

    const { getByTestId, findByRole, queryByRole } = render(ModelProviderMenu, {
      props: { pane },
    });
    await fireEvent.click(getByTestId('composer-model-menu-trigger'));

    await findByRole('menuitem', { name: /^Claude$/ });
    await waitFor(() => {
      expect(queryByRole('menuitem', { name: /Discussions/i })).toBeNull();
    });
  });

  it('keeps the Discussions entry visible on a fetch error, and surfaces the error inside the submenu', async () => {
    const pane = await buildPane(
      makeThread({ provider: 'claude', model: 'claude-sonnet-4-6' }),
    );
    setBindingMock('GetModelsForProvider', async () => []);
    setBindingMock('ListDiscussionsForThread', async () => {
      throw new Error('db offline');
    });

    const { getByTestId, findByRole, findByTestId } = render(ModelProviderMenu, {
      props: { pane },
    });
    await fireEvent.click(getByTestId('composer-model-menu-trigger'));

    const discussionsRow = await findByRole('menuitem', { name: /Discussions/i });
    await fireEvent.click(discussionsRow);

    const errorEl = await findByTestId('discussions-submenu-error');
    expect(errorEl.textContent).toBe('db offline');
  });

  it('hides the Discussions entry for a draft/unstarted thread without calling the binding', async () => {
    const pane = await buildPane(makeThread({ id: '', provider: 'claude', model: 'claude-sonnet-4-6' }));
    setBindingMock('GetModelsForProvider', async () => []);
    const listDiscussions = setBindingMock('ListDiscussionsForThread', async () => [architects]);

    const { getByTestId, findByRole, queryByRole } = render(ModelProviderMenu, {
      props: { pane },
    });
    await fireEvent.click(getByTestId('composer-model-menu-trigger'));

    await findByRole('menuitem', { name: /^Claude$/ });
    expect(queryByRole('menuitem', { name: /Discussions/i })).toBeNull();
    expect(listDiscussions).not.toHaveBeenCalled();
  });

  // Regression: a draft placeholder has a non-empty synthetic id
  // (`draft:<paneId>:<projectId>:<mode>:<ts>`), so the old empty-id guard
  // in ensureDiscussions let the fetch through. The backend's GetThread
  // then failed with "sql: no rows in result set", and the error branch
  // of showDiscussions forced the Discussions entry visible just to
  // display that error. The guard must key on pane.threadId (null for
  // placeholders), not pane.thread.id.
  it('hides the Discussions entry for a draft placeholder without calling the binding', async () => {
    const pane = await buildPane(makeThread({ provider: 'claude', model: 'claude-sonnet-4-6' }));
    pane.startDraftPlaceholder({
      id: 'project-1',
      path: '/tmp',
      name: 'Project One',
      sortPosition: 0,
      createdAt: 0,
      updatedAt: 0,
      archived: false,
    });
    expect(pane.thread?.id ?? '').toMatch(/^draft:/);
    setBindingMock('GetModelsForProvider', async () => []);
    const listDiscussions = setBindingMock('ListDiscussionsForThread', async () => {
      throw new Error('store: get thread draft:...: sql: no rows in result set');
    });

    const { getByTestId, findByRole, queryByRole } = render(ModelProviderMenu, {
      props: { pane },
    });
    await fireEvent.click(getByTestId('composer-model-menu-trigger'));

    await findByRole('menuitem', { name: /^Claude$/ });
    await waitFor(() => {
      expect(queryByRole('menuitem', { name: /Discussions/i })).toBeNull();
    });
    expect(listDiscussions).not.toHaveBeenCalled();
  });

  it('a discussion favorite is hidden when definitions are empty and visible once they exist', async () => {
    const pane = await buildPane(makeThread({ provider: 'claude', model: 'claude-opus-4-7' }));
    setBindingMock('GetModelsForProvider', async () => []);
    setBindingMock('ListChatBarFavorites', async () => [
      {
        kind: 'discussion',
        provider: '',
        value: 'architects',
        label: 'Architects',
        createdAt: 2,
      },
    ]);
    setBindingMock('ListDiscussionsForThread', async () => []);

    const { getByTestId, findByRole, queryByRole } = render(ModelProviderMenu, {
      props: { pane },
    });
    await fireEvent.click(getByTestId('composer-model-menu-trigger'));
    await findByRole('menuitem', { name: /^Claude$/ });
    expect(queryByRole('menuitem', { name: /Architects/i })).toBeNull();

    // Close, reopen with a definition now present — ensureDiscussions
    // refetches on every open (no loaded-once flag), so the favorite
    // should reappear without remounting the component.
    await fireEvent.click(getByTestId('composer-model-menu-trigger'));
    setBindingMock('ListDiscussionsForThread', async () => [architects]);
    await fireEvent.click(getByTestId('composer-model-menu-trigger'));
    await findByRole('menuitem', { name: /Architects/i });
  });
});

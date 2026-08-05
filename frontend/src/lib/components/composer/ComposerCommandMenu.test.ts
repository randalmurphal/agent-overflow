// Composer command menu, end to end through the real component: what the `/`
// menu offers where, what a selection puts in the draft, what a send does with
// the leading word, and which commands never reach a provider at all.
//
// Separate file from Composer.test.ts on purpose — that one is already the
// composer's send/upload/queue suite, and this is its own feature surface.

import { beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, waitFor } from '@testing-library/svelte';
import { tick } from 'svelte';
import Composer from './Composer.svelte';
import {
  createComposerDraftStore,
  resetComposerDraftSnapshotsForTest,
} from '../../stores/composerDraft.svelte';
import { buildPane, makeThread } from '../../../test/helpers/chat';
import { getBindingMock, resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { applyProviderCommands } from '../../stores/providerCommands.svelte';
import { resetForTest as resetClaudeSkills } from '../../stores/claudeSkills.svelte';
import { resetForTest as resetCodexSkills } from '../../stores/codexSkills.svelte';
import {
  projectTurnStarted,
  resetForTest as resetThreadStatuses,
} from '../../stores/threadStatuses.svelte';
import { resetPanesForTest } from '../../stores/panes.svelte';
import { resetPaneLayoutForTest } from '../../stores/paneLayout.svelte';
import { resetCompanionPanesForTest } from '../../stores/companionPanes.svelte';
import { resetForTest as resetWorktreeIntent } from '../../stores/worktreeIntent.svelte';
import { resetProposedPlanCacheForTests } from '../../stores/proposedPlans.svelte';
import { resetProviderModelsForTest } from '../../stores/providerModels.svelte';
import { resetComposerPickerRegistryForTest, registerComposerPicker } from '../../stores/composerPickerRegistry.svelte';
import {
  registerPaneTitleRename,
  resetPaneTitleRenameForTest,
} from '../../stores/paneTitleRename';
import type { ThreadPane } from '../../stores/thread.svelte';

function installBaseMocks() {
  setBindingMock('GetDraft', async (threadId: string) => ({
    threadId,
    content: '',
    attachmentIds: [],
    terminalChips: [],
    updatedAt: 0,
  }));
  setBindingMock('SaveDraft', async () => {});
  setBindingMock('ClearDraft', async () => {});
  setBindingMock('DeleteEmptyDraftThread', async () => false);
  setBindingMock('ListAttachments', async () => []);
  setBindingMock('ListLiveBackgroundTasks', async () => []);
  setBindingMock('ListThreadProposedPlans', async () => []);
  setBindingMock('ListProposedPlanComments', async () => []);
  setBindingMock('SearchWorkspaceFiles', async () => ({
    files: [],
    truncated: false,
    root: '/tmp/workspace',
  }));
  setBindingMock('GetClaudeSlashCommands', async () => ({ probed: false, commands: [] }));
  setBindingMock('GetCodexSkills', async () => ({ cwd: '/tmp/workspace', skills: [], errors: [] }));
  setBindingMock('GetClaudeSkills', async () => []);
  setBindingMock('SendMessageWithOptions', async () => makeThread());
  // The model catalog the picker — and therefore `/model <arg>` — resolves
  // against. Two Opus rows on purpose: the ambiguity case is real.
  setBindingMock('GetModelsForProvider', async (provider: string) =>
    provider === 'claude'
      ? [
          { slug: 'claude-opus-5', name: 'Opus 5', capabilities: [] },
          { slug: 'claude-sonnet-4-6', name: 'Sonnet 4.6', capabilities: [] },
        ]
      : [{ slug: 'gpt-5.6-codex', name: 'GPT-5.6 Codex', capabilities: [] }],
  );
}

async function buildDraft(threadId: string | null = 'thread-1') {
  const draft = createComposerDraftStore({ debounceMs: 0 });
  await draft.setThread(threadId);
  return draft;
}

async function mountComposer(pane: ThreadPane) {
  const draft = await buildDraft(pane.threadId);
  const rendered = render(Composer, { props: { pane, draft } });
  const textarea = rendered.getByLabelText('Message Input') as HTMLTextAreaElement;
  await waitFor(() => expect(textarea.disabled).toBe(false));
  return { ...rendered, draft, textarea };
}

// fireEvent.input alone leaves selectionStart at 0 in happy-dom, and the
// trigger reads the caret — so type like a human does: value, then caret.
async function typeInto(textarea: HTMLTextAreaElement, value: string) {
  await fireEvent.input(textarea, { target: { value } });
  textarea.setSelectionRange(value.length, value.length);
  await fireEvent.select(textarea);
  await tick();
}

function getBindingMockCalled(name: string): boolean {
  return (getBindingMock(name)?.mock.calls.length ?? 0) > 0;
}

function optionNames(getAll: (id: string) => HTMLElement[]): string[] {
  return getAll('slash-option').map((el) => el.getAttribute('data-command') ?? '');
}

describe('composer command menu', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetComposerDraftSnapshotsForTest();
    resetProposedPlanCacheForTests();
    resetWorktreeIntent();
    resetThreadStatuses();
    resetPanesForTest();
    resetPaneLayoutForTest();
    resetCompanionPanesForTest();
    resetCodexSkills();
    resetClaudeSkills();
    resetProviderModelsForTest();
    resetComposerPickerRegistryForTest();
    resetPaneTitleRenameForTest();
    installBaseMocks();
  });

  describe('what the menu offers, and where', () => {
    it('offers provider commands only at the start of the draft', async () => {
      setBindingMock('GetClaudeSlashCommands', async () => ({
        probed: true,
        commands: [{ name: 'usage', description: 'Show usage', argumentHint: '[period]' }],
      }));
      const pane = await buildPane(makeThread({ provider: 'claude' }));
      const { textarea, getAllByTestId, queryByTestId } = await mountComposer(pane);

      await typeInto(textarea, '/');
      await waitFor(() =>
        expect(optionNames(getAllByTestId)).toContain('usage'),
      );
      expect(optionNames(getAllByTestId)).toEqual(
        expect.arrayContaining(['workflow', 'model', 'usage']),
      );
      // Description and argument hint both render.
      const usage = getAllByTestId('slash-option').find(
        (el) => el.getAttribute('data-command') === 'usage',
      )!;
      expect(usage.textContent).toContain('Show usage');
      expect(usage.textContent).toContain('[period]');

      // Mid-text, only AO's own any-position commands remain.
      await typeInto(textarea, 'before we start /');
      await waitFor(() => expect(queryByTestId('slash-popover')).not.toBeNull());
      expect(optionNames(getAllByTestId)).toEqual(['workflow']);
    });

    it('renders nothing provider-specific while the probe is unknown', async () => {
      const pane = await buildPane(makeThread({ provider: 'claude' }));
      const { textarea, getAllByTestId } = await mountComposer(pane);

      await typeInto(textarea, '/');
      await waitFor(() => expect(getAllByTestId('slash-option').length).toBeGreaterThan(0));
      // probed:false is UNKNOWN, never "this binary has none" — so the menu
      // shows AO's own rows and no empty "Provider commands" section.
      expect(optionNames(getAllByTestId)).not.toContain('usage');
      expect(
        getAllByTestId('slash-section-header').map((el) => el.getAttribute('data-section')),
      ).toEqual(['ao']);
    });

    it('lets a session frame replace the probe list wholesale, enriched by it', async () => {
      setBindingMock('GetClaudeSlashCommands', async () => ({
        probed: true,
        commands: [
          { name: 'usage', description: 'Show usage' },
          { name: 'gone', description: 'Only the probe knows this' },
        ],
      }));
      const pane = await buildPane(makeThread({ provider: 'claude' }));
      const { textarea, getAllByTestId } = await mountComposer(pane);

      applyProviderCommands({
        threadId: 'thread-1',
        provider: 'claude',
        replace: true,
        commands: [{ name: 'usage' }, { name: 'mcp__linear__issue' }],
      });

      await typeInto(textarea, '/');
      await waitFor(() => expect(optionNames(getAllByTestId)).toContain('usage'));
      const names = optionNames(getAllByTestId);
      expect(names).toContain('mcp__linear__issue');
      // The frame's name set is authoritative: a probe-only name is dropped.
      expect(names).not.toContain('gone');
      // …but the probe's description still enriches a name they share.
      const usage = getAllByTestId('slash-option').find(
        (el) => el.getAttribute('data-command') === 'usage',
      )!;
      expect(usage.textContent).toContain('Show usage');
    });

    it('inserts a provider command as plain text, slash included', async () => {
      setBindingMock('GetClaudeSlashCommands', async () => ({
        probed: true,
        commands: [{ name: 'usage', description: 'Show usage' }],
      }));
      const pane = await buildPane(makeThread({ provider: 'claude' }));
      const { textarea, draft, getAllByTestId } = await mountComposer(pane);

      await typeInto(textarea, '/usa');
      await waitFor(() => expect(optionNames(getAllByTestId)).toContain('usage'));
      await fireEvent.click(
        getAllByTestId('slash-option').find((el) => el.getAttribute('data-command') === 'usage')!,
      );
      await tick();

      expect(textarea.value).toBe('/usage ');
      expect(draft.content).toBe('/usage ');
    });
  });

  describe('active-row reveal', () => {
    async function mountOpenMenu() {
      const pane = await buildPane(makeThread({ provider: 'claude' }));
      const mounted = await mountComposer(pane);
      await typeInto(mounted.textarea, '/');
      await waitFor(() => expect(mounted.getAllByTestId('slash-option').length).toBeGreaterThan(1));
      return mounted;
    }

    function activeOption(getAll: (id: string) => HTMLElement[]): HTMLElement {
      return getAll('slash-option').find((el) => el.getAttribute('aria-selected') === 'true')!;
    }

    it('scrolls the active row into view when the arrow keys move it', async () => {
      const { textarea, getAllByTestId } = await mountOpenMenu();
      const scrolled: string[] = [];
      for (const el of getAllByTestId('slash-option')) {
        el.scrollIntoView = () => scrolled.push(el.getAttribute('data-command') ?? '');
      }

      await fireEvent.keyDown(textarea, { key: 'ArrowDown' });
      await tick();

      expect(activeOption(getAllByTestId).getAttribute('data-command')).toBe(scrolled.at(-1));
      expect(scrolled.length).toBe(1);
    });

    it('moves the highlight on mousemove without scrolling the list', async () => {
      const { getAllByTestId } = await mountOpenMenu();
      const scrolled: string[] = [];
      for (const el of getAllByTestId('slash-option')) {
        el.scrollIntoView = () => scrolled.push(el.getAttribute('data-command') ?? '');
      }
      const last = getAllByTestId('slash-option').at(-1)!;

      await fireEvent.mouseMove(last);
      await tick();

      expect(activeOption(getAllByTestId)).toBe(last);
      expect(scrolled).toEqual([]);
    });
  });

  describe('Codex skills', () => {
    it('inserts a skill with Codex’s own $ token', async () => {
      setBindingMock('GetCodexSkills', async () => ({
        cwd: '/tmp/workspace',
        skills: [
          {
            name: 'review-code',
            description: 'model-facing',
            shortDescription: 'Careful review',
            path: '/s/SKILL.md',
            scope: 'repo',
            enabled: true,
          },
        ],
        errors: [],
      }));
      const pane = await buildPane(makeThread({ provider: 'codex' }));
      const { textarea, draft, getAllByTestId } = await mountComposer(pane);

      await typeInto(textarea, '/rev');
      await waitFor(() => expect(optionNames(getAllByTestId)).toContain('review-code'));
      await fireEvent.click(
        getAllByTestId('slash-option').find(
          (el) => el.getAttribute('data-command') === 'review-code',
        )!,
      );
      await tick();

      expect(textarea.value).toBe('$review-code ');
      expect(draft.content).toBe('$review-code ');
    });

    it('marks a disabled skill and refuses to insert it', async () => {
      setBindingMock('GetCodexSkills', async () => ({
        cwd: '/tmp/workspace',
        skills: [
          {
            name: 'legacy',
            description: 'Old',
            path: '/s/SKILL.md',
            scope: 'user',
            enabled: false,
          },
        ],
        errors: [],
      }));
      const pane = await buildPane(makeThread({ provider: 'codex' }));
      const { textarea, draft, getAllByTestId } = await mountComposer(pane);

      await typeInto(textarea, '/leg');
      await waitFor(() => expect(optionNames(getAllByTestId)).toContain('legacy'));
      const row = getAllByTestId('slash-option').find(
        (el) => el.getAttribute('data-command') === 'legacy',
      )! as HTMLButtonElement;
      expect(row.getAttribute('data-disabled')).toBe('true');
      expect(row.disabled).toBe(true);

      await fireEvent.click(row);
      await tick();
      expect(draft.content).toBe('/leg');
    });

    it('degrades quietly when the local-only skills read is refused', async () => {
      setBindingMock('GetCodexSkills', async () => {
        throw new Error('local only');
      });
      const pane = await buildPane(makeThread({ provider: 'codex' }));
      const { textarea, getAllByTestId } = await mountComposer(pane);

      await typeInto(textarea, '/');
      await waitFor(() => expect(getAllByTestId('slash-option').length).toBeGreaterThan(0));
      // No skills section, no unhandled rejection, AO's own rows still there.
      expect(
        getAllByTestId('slash-section-header').map((el) => el.getAttribute('data-section')),
      ).toEqual(['ao']);
      expect(optionNames(getAllByTestId)).toContain('workflow');
    });
  });

  describe('send-time classification', () => {
    it('marks a hand-typed provider command without any menu interaction', async () => {
      setBindingMock('GetClaudeSlashCommands', async () => ({
        probed: true,
        commands: [{ name: 'usage' }],
      }));
      const send = setBindingMock('SendMessageWithOptions', async () => makeThread());
      const pane = await buildPane(makeThread({ provider: 'claude' }));
      const { textarea, getByTestId } = await mountComposer(pane);

      // Open the menu once so the probe list is loaded, then type past it.
      await typeInto(textarea, '/');
      await waitFor(() => expect(optionNames).toBeTruthy());
      await typeInto(textarea, '/usage for this week');
      await fireEvent.click(getByTestId('composer-send'));

      await waitFor(() =>
        expect(send).toHaveBeenCalledWith(
          'thread-1',
          '/usage for this week',
          expect.objectContaining({ providerCommand: true }),
        ),
      );
    });

    it('leaves an unknown /word guarded', async () => {
      setBindingMock('GetClaudeSlashCommands', async () => ({
        probed: true,
        commands: [{ name: 'usage' }],
      }));
      const send = setBindingMock('SendMessageWithOptions', async () => makeThread());
      const pane = await buildPane(makeThread({ provider: 'claude' }));
      const { textarea, getByTestId } = await mountComposer(pane);

      await typeInto(textarea, '/deploy the release');
      await fireEvent.click(getByTestId('composer-send'));

      await waitFor(() => expect(send).toHaveBeenCalled());
      // The generated options class declares every field, so the property
      // exists on the instance — what matters is that it is not set.
      expect((send.mock.calls[0][2] as { providerCommand?: boolean }).providerCommand)
        .toBeFalsy();
    });
  });

  describe('intercepted commands', () => {
    it('consumes /clear without sending or persisting it', async () => {
      const send = setBindingMock('SendMessageWithOptions', async () => makeThread());
      const pane = await buildPane(makeThread({ provider: 'claude' }));
      const { textarea, draft, getByTestId } = await mountComposer(pane);

      await typeInto(textarea, '/clear');
      await fireEvent.click(getByTestId('composer-send'));
      await tick();

      expect(send).not.toHaveBeenCalled();
      expect(draft.content).toBe('');
    });

    it('renames the thread through the existing binding on /rename <text>', async () => {
      const rename = setBindingMock('RenameThread', async () => undefined);
      setBindingMock('GetThread', async () => makeThread({ title: 'Ship the parser' }));
      const send = setBindingMock('SendMessageWithOptions', async () => makeThread());
      const pane = await buildPane(makeThread({ provider: 'claude' }));
      const { textarea, getByTestId } = await mountComposer(pane);

      await typeInto(textarea, '/rename Ship the parser');
      await fireEvent.click(getByTestId('composer-send'));

      await waitFor(() => expect(rename).toHaveBeenCalledWith('thread-1', 'Ship the parser'));
      expect(send).not.toHaveBeenCalled();
    });

    it('opens the pane’s own rename editor on a bare /rename', async () => {
      const start = vi.fn();
      const pane = await buildPane(makeThread({ provider: 'claude' }));
      registerPaneTitleRename(pane.paneId, { start });
      const { textarea, getByTestId } = await mountComposer(pane);

      await typeInto(textarea, '/rename');
      await fireEvent.click(getByTestId('composer-send'));
      await tick();

      expect(start).toHaveBeenCalled();
    });

    it('opens the model picker on a bare /model', async () => {
      const open = vi.fn();
      const pane = await buildPane(makeThread({ provider: 'claude' }));
      const { textarea, getByTestId } = await mountComposer(pane);
      // Registered after mount so it wins over the toolbar's own handle.
      registerComposerPicker(pane.paneId, 'model', { isOpen: () => false, open, close: () => {} });

      await typeInto(textarea, '/model');
      await fireEvent.click(getByTestId('composer-send'));
      await tick();

      expect(open).toHaveBeenCalled();
    });

    it('applies /model <arg> through the picker’s own path', async () => {
      const update = setBindingMock('UpdateThreadModelSelection', async () =>
        makeThread({ model: 'claude-opus-5' }),
      );
      const pane = await buildPane(makeThread({ provider: 'claude' }));
      const { textarea, getByTestId } = await mountComposer(pane);
      // The toolbar warms the catalog on mount; wait for it before resolving.
      await waitFor(() => expect(getBindingMockCalled('GetModelsForProvider')).toBe(true));

      await typeInto(textarea, '/model opus');
      await fireEvent.click(getByTestId('composer-send'));

      await waitFor(() =>
        expect(update).toHaveBeenCalledWith('thread-1', 'claude', 'claude-opus-5'),
      );
    });

    it('reports a composer-local error when /model names nothing', async () => {
      const pane = await buildPane(makeThread({ provider: 'claude' }));
      const { textarea, getByTestId, findByTestId } = await mountComposer(pane);
      await waitFor(() => expect(getBindingMockCalled('GetModelsForProvider')).toBe(true));

      await typeInto(textarea, '/model definitely-not-a-model');
      await fireEvent.click(getByTestId('composer-send'));

      const error = await findByTestId('composer-command-error');
      expect(error.textContent).toMatch(/definitely-not-a-model/);
      expect(error.textContent).toMatch(/Opus 5/);
    });

    it('takes precedence over a provider command of the same name', async () => {
      setBindingMock('GetClaudeSlashCommands', async () => ({
        probed: true,
        commands: [{ name: 'model', description: 'Claude’s own picker' }],
      }));
      const send = setBindingMock('SendMessageWithOptions', async () => makeThread());
      const open = vi.fn();
      const pane = await buildPane(makeThread({ provider: 'claude' }));
      const { textarea, getAllByTestId, getByTestId } = await mountComposer(pane);
      registerComposerPicker(pane.paneId, 'model', { isOpen: () => false, open, close: () => {} });

      await typeInto(textarea, '/mod');
      await waitFor(() => expect(optionNames(getAllByTestId)).toContain('model'));
      // Exactly one `/model` row, and it is the app-side reroute.
      const rows = getAllByTestId('slash-option').filter(
        (el) => el.getAttribute('data-command') === 'model',
      );
      expect(rows).toHaveLength(1);
      expect(rows[0].getAttribute('data-kind')).toBe('intercepted');

      await typeInto(textarea, '/model');
      await fireEvent.click(getByTestId('composer-send'));
      await tick();
      expect(open).toHaveBeenCalled();
      expect(send).not.toHaveBeenCalled();
    });
  });

  describe('Codex /compact and /review', () => {
    it('offers them only on a Codex thread', async () => {
      const claudePane = await buildPane(makeThread({ provider: 'claude' }));
      const claude = await mountComposer(claudePane);
      await typeInto(claude.textarea, '/');
      await waitFor(() => expect(claude.getAllByTestId('slash-option').length).toBeGreaterThan(0));
      expect(optionNames(claude.getAllByTestId)).not.toContain('compact');
      claude.unmount();

      const codexPane = await buildPane(makeThread({ provider: 'codex' }), [], 'codex-pane');
      const codex = await mountComposer(codexPane);
      await typeInto(codex.textarea, '/');
      await waitFor(() => expect(optionNames(codex.getAllByTestId)).toContain('compact'));
      expect(optionNames(codex.getAllByTestId)).toContain('review');
    });

    it('compacts through the RPC and never sends the text', async () => {
      const compact = setBindingMock('CompactCodexThread', async () => undefined);
      const send = setBindingMock('SendMessageWithOptions', async () => makeThread());
      const pane = await buildPane(makeThread({ provider: 'codex' }));
      const { textarea, getByTestId } = await mountComposer(pane);

      await typeInto(textarea, '/compact');
      await fireEvent.click(getByTestId('composer-send'));

      await waitFor(() => expect(compact).toHaveBeenCalledWith('thread-1'));
      expect(send).not.toHaveBeenCalled();
    });

    it('refuses /compact while a turn is running — the turn is not steerable', async () => {
      const compact = setBindingMock('CompactCodexThread', async () => undefined);
      const pane = await buildPane(makeThread({ provider: 'codex' }));
      const { textarea, getByTestId, findByTestId } = await mountComposer(pane);
      projectTurnStarted('thread-1', 't1', 0, Date.now());

      await typeInto(textarea, '/compact');
      await fireEvent.click(getByTestId('composer-send'));

      const error = await findByTestId('composer-command-error');
      expect(error.textContent).toMatch(/idle thread/);
      expect(compact).not.toHaveBeenCalled();
    });

    it('completes /review targets from the git surfaces and starts the matching review', async () => {
      setBindingMock('GitListBranches', async () => [
        { name: 'main', isCurrent: false, isDefault: true },
      ]);
      setBindingMock('ListBranchCommits', async () => [
        { sha: 'abc1234000', shortSha: 'abc1234', subject: 'Fix the parser', author: 'a', authoredAt: 0 },
      ]);
      const start = setBindingMock('StartCodexReview', async () => ({
        threadId: 'thread-1',
        turnId: 'turn-1',
        turnStatus: 'running',
      }));
      const pane = await buildPane(makeThread({ provider: 'codex' }));
      const { textarea, draft, getAllByTestId, getByTestId } = await mountComposer(pane);

      await typeInto(textarea, '/review ');
      await waitFor(() => expect(optionNames(getAllByTestId)).toContain('commit abc1234'));
      expect(optionNames(getAllByTestId)).toEqual(
        expect.arrayContaining(['uncommitted', 'custom', 'branch main', 'commit abc1234']),
      );
      const commitRow = getAllByTestId('slash-option').find(
        (el) => el.getAttribute('data-command') === 'commit abc1234',
      )!;
      expect(commitRow.textContent).toContain('Fix the parser');

      await fireEvent.click(commitRow);
      await tick();
      expect(draft.content).toBe('/review commit abc1234 ');

      await fireEvent.click(getByTestId('composer-send'));
      await waitFor(() =>
        expect(start).toHaveBeenCalledWith(
          'thread-1',
          expect.objectContaining({ kind: 'commit', sha: 'abc1234' }),
        ),
      );
    });

    it('surfaces an RPC failure instead of swallowing it', async () => {
      setBindingMock('GitListBranches', async () => []);
      setBindingMock('StartCodexReview', async () => {
        throw new Error('no active session for thread');
      });
      const pane = await buildPane(makeThread({ provider: 'codex' }));
      const { textarea, getByTestId, findByTestId } = await mountComposer(pane);

      await typeInto(textarea, '/review');
      await fireEvent.click(getByTestId('composer-send'));

      const error = await findByTestId('composer-command-error');
      expect(error.textContent).toMatch(/no active session/);
    });
  });
});

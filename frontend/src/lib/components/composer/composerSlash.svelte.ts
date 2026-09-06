import { withBackendTarget } from '../../transport/backends';
import { composeWorkspaceKey } from '../../utils/workspaceKey';
import { threadMachine } from '../../stores/attachedBackends.svelte';
// Composer command-menu state.
//
// Owns: which trigger is open (the `/name` completion, or `/review`'s target
// completion), the filtered sections that implies, the active row, the lazily
// loaded sources those rows come from, and the composer-local error an
// intercepted command reports.
//
// Rendering lives in Composer.svelte / ComposerSlashPopover.

import { GitListBranches, ListRecentCommits } from '../../stores/bindings';
import { getClaudeSkills, ensureClaudeSkills } from '../../stores/claudeSkills.svelte';
import { getCodexSkills, ensureCodexSkills } from '../../stores/codexSkills.svelte';
import {
  ensureClaudeProbeCommands,
  getClaudeProbeCommands,
  getProviderCommandsFrame,
} from '../../stores/providerCommands.svelte';
import { paneWorkspacePath, type ThreadPane } from '../../stores/thread.svelte';
import type { BranchCommit, GitBranch, WorkspaceRef } from '../../types/git';
import { errString } from '../../utils/errors';
import { runInterceptedCommand } from './composerCommandActions';
import {
  buildCommandSections,
  filterCommandSections,
  flattenSections,
  interceptedCommandNames,
  isProviderTurnCommand,
  type ComposerCommandEntry,
  type ComposerCommandSection,
} from './composerCommandEntries';
import {
  interceptedCommandRange,
  parseInterceptedCommand,
} from './composerCommandParse';
import {
  detectCommandTrigger,
  detectReviewTargetTrigger,
} from './composerCommandTrigger';
import { buildReviewSections } from './composerReviewTargets';
import { replaceTextareaRange } from './textareaEdit';

export interface ComposerSlashOptions {
  /** Returns the textarea DOM element. May be undefined before mount. */
  getTextarea: () => HTMLTextAreaElement | undefined;
  /**
   * Getter, not the pane itself: the caller reads it off `$props()`, and
   * capturing that value at construction would freeze this handle to whichever
   * pane was mounted first.
   */
  getPane: () => ThreadPane;
}

interface OpenTrigger {
  level: 'command' | 'review';
  query: string;
  start: number;
  end: number;
  atStart: boolean;
}

interface ReviewGitData {
  /** The checkout the rows were read from — the staleness key. */
  key: string;
  branches: GitBranch[];
  commits: BranchCommit[];
  loading: boolean;
  error: string;
}

export interface ComposerSlashHandle {
  readonly slashTrigger: OpenTrigger | null;
  /**
   * Whether the menu should be visible. A trigger with no matching rows stays
   * OPEN as state but shows nothing: that is what leaves `/workflowish` as
   * plain text while a later backspace re-reveals the menu without the user
   * having to retype the slash.
   */
  readonly slashOpen: boolean;
  readonly slashSections: ComposerCommandSection[];
  readonly slashResults: ComposerCommandEntry[];
  readonly slashActiveIndex: number;
  readonly commandError: string;
  setSlashActiveIndex(i: number): void;

  /** Inspect the textarea's value + caret and open / filter / close the menu. */
  refreshTrigger(): void;

  insertCommand(entry: ComposerCommandEntry): void;
  closeSlash(): void;
  clearCommandError(): void;

  /**
   * Consume the draft if it invokes an intercepted command. Returns true when
   * the composer must NOT send — the text has already been handled.
   */
  consumeInterceptedSend(message: string): boolean;
  /**
   * Accent-overlay range of a leading intercepted command, for the same
   * treatment AO's own command words get. Empty list when there is none.
   */
  interceptedRanges(value: string): { name: string; start: number; end: number }[];
}

export function createComposerSlash(opts: ComposerSlashOptions): ComposerSlashHandle {
  const pane = $derived(opts.getPane());

  let trigger: OpenTrigger | null = $state(null);
  let slashActiveIndex = $state(0);
  let commandError = $state('');
  // Set while the menu is deliberately dismissed (Escape) for a draft that
  // still matches. Cleared as soon as the draft stops matching, so the next
  // `/` types a fresh menu rather than staying suppressed forever.
  let dismissed = $state(false);
  let reviewGit = $state.raw<ReviewGitData | null>(null);

  const workspacePath = $derived(paneWorkspacePath(pane));
  // `/review` reads branches and commits out of a CHECKOUT, so a draft
  // placeholder answers the same rows a persisted thread does.
  const workspace = $derived<WorkspaceRef | null>(pane.workspace);
  const backend = $derived(threadMachine(pane.threadId ?? '', pane.thread?.projectId));
  const reviewKey = $derived(composeWorkspaceKey(backend, workspace?.workspacePath ?? ''));
  const provider = $derived(pane.thread?.provider ?? '');

  const interceptedNames = $derived(interceptedCommandNames(provider));

  const slashSections = $derived.by(() => {
    if (!trigger) return [];
    if (trigger.level === 'review') {
      const git = reviewGit?.key === reviewKey ? reviewGit : null;
      return filterCommandSections(
        buildReviewSections({
          branches: git?.branches ?? [],
          commits: git?.commits ?? [],
          loading: git?.loading ?? true,
          error: git?.error ?? '',
        }),
        trigger.query,
      );
    }
    const frame = getProviderCommandsFrame(pane.threadId);
    const probe = getClaudeProbeCommands(threadMachine(pane.threadId ?? '', pane.thread?.projectId));
    const sections = buildCommandSections({
      provider,
      atStart: trigger.atStart,
      sessionCommands: frame ? frame.commands : null,
      probeCommands: probe.probed ? probe.commands : null,
      skills: getCodexSkills(workspacePath, threadMachine(pane.threadId ?? '', pane.thread?.projectId)).skills,
      claudeSkills: getClaudeSkills(workspacePath, threadMachine(pane.threadId ?? '', pane.thread?.projectId)).skills,
    });
    return filterCommandSections(sections, trigger.query);
  });

  const slashResults = $derived(flattenSections(slashSections));

  function closeSlash(): void {
    trigger = null;
    slashActiveIndex = 0;
    dismissed = true;
  }

  function warmSources(next: OpenTrigger): void {
    if (next.level === 'review') {
      void loadReviewGit();
      return;
    }
    if (!next.atStart) return;
    if (provider === 'codex') {
      void ensureCodexSkills(workspacePath, false, threadMachine(pane.threadId ?? '', pane.thread?.projectId));
    } else if (provider !== '') {
      void ensureClaudeProbeCommands(threadMachine(pane.threadId ?? '', pane.thread?.projectId));
      void ensureClaudeSkills(workspacePath, threadMachine(pane.threadId ?? '', pane.thread?.projectId));
    }
  }

  async function loadReviewGit(): Promise<void> {
    const ws = workspace;
    if (!ws) return;
    const key = reviewKey;
    const target = backend;
    if (reviewGit?.key === key) return;
    reviewGit = { key, branches: [], commits: [], loading: true, error: '' };
    try {
      // Commits are the workspace's recent commits (plain `git log` from
      // HEAD) — the same source codex's own review picker uses — NOT the
      // review pane's `base..HEAD` list, which is empty for a thread
      // sitting on the default branch.
      const [branches, commits] = await Promise.all([
        withBackendTarget(target, () => GitListBranches(ws)).then((b) => (b ?? []) as GitBranch[]),
        withBackendTarget(target, () => ListRecentCommits(ws)).then((c) => (c ?? []) as BranchCommit[]),
      ]);
      if (reviewKey !== key) return;
      reviewGit = { key, branches, commits, loading: false, error: '' };
    } catch (err) {
      if (reviewKey !== key) return;
      reviewGit = {
        key,
        branches: [],
        commits: [],
        loading: false,
        error: errString(err),
      };
    }
  }

  function refreshTrigger(): void {
    const textarea = opts.getTextarea();
    if (!textarea) return;
    const value = textarea.value;
    const caret = textarea.selectionStart ?? value.length;

    // `/review …` is a second completion level over the SAME text, so it is
    // tried first: once the command word is settled, the word-scoped `/`
    // trigger has already closed and only the target trigger can be open.
    const review = interceptedNames.has('review')
      ? detectReviewTargetTrigger(value, caret)
      : null;
    const next: OpenTrigger | null = review
      ? { level: 'review', ...review, atStart: true }
      : (() => {
          const command = detectCommandTrigger(value, caret);
          return command ? { level: 'command' as const, ...command } : null;
        })();

    if (!next) {
      trigger = null;
      slashActiveIndex = 0;
      dismissed = false;
      return;
    }
    if (dismissed) return;
    // The active row is clamped on READ, not here: `slashResults` derives from
    // `trigger`, so it still holds the previous trigger's rows at this point.
    trigger = next;
    warmSources(next);
  }

  function insertCommand(entry: ComposerCommandEntry): void {
    const textarea = opts.getTextarea();
    if (!trigger || !textarea || entry.disabled) return;
    replaceTextareaRange(textarea, trigger.start, trigger.end, entry.insertText);
    trigger = null;
    slashActiveIndex = 0;
    dismissed = false;
  }

  function consumeInterceptedSend(message: string): boolean {
    const invocation = parseInterceptedCommand(message, interceptedNames);
    if (!invocation) return false;
	if (isProviderTurnCommand(provider, invocation.name)) return false;
    commandError = '';
    closeSlash();
    // The menu is closed and the text is already gone by the time the action
    // resolves; only its failure needs somewhere to land.
    void runInterceptedCommand(pane, invocation).then((result) => {
      if (result.error !== '') commandError = result.error;
    });
    return true;
  }

  return {
    get slashTrigger() { return trigger; },
    get slashOpen() { return trigger !== null && slashResults.length > 0; },
    get slashSections() { return slashSections; },
    get slashResults() { return slashResults; },
    get slashActiveIndex() {
      const count = slashResults.length;
      if (count === 0) return 0;
      return Math.min(slashActiveIndex, count - 1);
    },
    get commandError() { return commandError; },
    setSlashActiveIndex(i: number): void { slashActiveIndex = i; },

    refreshTrigger,
    insertCommand,
    closeSlash,
    clearCommandError(): void { commandError = ''; },

    consumeInterceptedSend,
    interceptedRanges(value: string) {
      const range = interceptedCommandRange(value, interceptedNames);
      return range ? [range] : [];
    },
  };
}

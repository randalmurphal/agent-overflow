// The Merge section's method and commit message editor. The store holds
// the one selected method: the project's default (the first offered)
// until the reader picks, re-resolved on every snapshot, and changed only
// by a pick or by a snapshot that stops offering it. The editor has no
// method of its own; it shows the selected one, with the forge's default
// message for it, read when the editor opens and when the selection
// changes (never per tick), until the reader edits it. A merge sends the
// edited text exactly when it is edited and the selected method makes a
// commit (rebase and fast-forward make none); untouched, the forge's
// default applies as it does without the editor. A failed default read
// shows its sentence in the editor and never stands in the way of a
// merge. The editor belongs to the PR it opened on: on another PR it
// reads closed and sends nothing, and `discard` drops it with the
// selection.
//
// A selected method a snapshot stops offering falls back to the
// project's default through the same path as a pick, keeping edited text,
// with a notice saying what happened: the message now goes with the new
// method's commit, or, for a method that makes none, the editor stays
// open with the text and the merge sends none. A method that comes back
// does not move the selection back; a pick or an unedited message ends
// the notice.

import { untrack } from 'svelte';
import { GetPRMergeMessage } from './bindings';
import { captureError } from './errorReports.svelte';
import type { PRManageError } from './prManage.svelte';
import { withBackendTarget } from '../transport/backends';
import type { BackendKey } from '../transport/backendKey';
import type { MergeMessage } from '../types/models';
import { methodHasCommitMessage } from '../utils/prManage';
import { prReferenceWire, type PRRef } from '../utils/prReference';
import { userFacingError } from '../utils/userFacingError';

export interface PRMergeMessageHost {
  readonly prRef: PRRef | null;
  /** The PR's computer-scoped identity, or null. */
  readonly identity: string | null;
  readonly backend: BackendKey;
  /** The methods the snapshot offers, or null before one. */
  readonly methods: readonly string[] | null;
}

const METHOD_NAME: Record<string, string> = { merge: 'Merge', squash: 'Squash', rebase: 'Rebase', fast_forward: 'Fast-forward' };

function methodName(method: string): string {
  return METHOD_NAME[method] ?? method.charAt(0).toUpperCase() + method.slice(1);
}

/** What the editor says when `was` stopped being offered and the edited
 * text now goes with `next`. */
export function fallbackNotice(was: string, next: string): string {
  const what = methodHasCommitMessage(next)
    ? `This message will be used for the ${next === 'squash' ? 'squash' : 'merge'} commit.`
    : `${methodName(next)} makes no commit, so this message will not be used.`;
  return `${methodName(was)} is no longer allowed here. ${what}`;
}

export interface PRMergeMessageView {
  /** The method a merge uses and the editor shows: the reader's pick or
   * the fallback a snapshot forced, else the project's default. */
  readonly method: string;
  /** The reader picked `method` in the Merge menu: the editor follows,
   * reading its default unless the text was edited. */
  pick(method: string): void;
  readonly open: boolean;
  readonly title: string;
  readonly body: string;
  /** The reader changed the text; it is what a merge sends. */
  readonly edited: boolean;
  /** The default for the method is being read. */
  readonly loading: boolean;
  /** The default read failed; the editor starts empty. */
  readonly error: PRManageError | null;
  /** The selected method was no longer offered and the text moved to
   * another. */
  readonly notice: string | null;
  /** Opens the editor on the selected method. */
  openEditor(): void;
  setTitle(text: string): void;
  setBody(text: string): void;
  close(): void;
  /** What a merge sends as its commit message: the edited text while the
   * editor is open on this PR and the selected method makes a commit,
   * else nothing. */
  readonly commitMessage: MergeMessage | undefined;
  /** The PR changed: the editor closes and the selection is re-resolved. */
  discard(): void;
  dispose(): void;
}

export function createPRMergeMessageState(host: PRMergeMessageHost): PRMergeMessageView {
  let open = $state(false);
  // The PR the editor opened on.
  let openedOn = $state<{ ref: PRRef; key: string } | null>(null);
  // The selected method; null only before the first snapshot.
  let selected = $state<string | null>(null);
  let edited = $state<MergeMessage | null>(null);
  // The defaults read while this editor was open, per method.
  let defaults = $state.raw<ReadonlyMap<string, MergeMessage>>(new Map());
  let loading = $state(false);
  let error = $state<PRManageError | null>(null);
  let notice = $state<string | null>(null);
  let generation = 0;

  const live = $derived(open && openedOn !== null && openedOn.key === host.identity);
  const method = $derived(selected ?? 'merge');

  async function read(forMethod: string): Promise<void> {
    const ref = openedOn?.ref;
    if (!ref || !methodHasCommitMessage(forMethod)) return;
    if (defaults.has(forMethod)) {
      error = null;
      return;
    }
    const mine = ++generation;
    loading = true;
    error = null;
    try {
      const answer = await withBackendTarget(host.backend, () => GetPRMergeMessage(prReferenceWire(ref), forMethod));
      if (mine !== generation) return;
      defaults = new Map([...defaults, [forMethod, { title: answer.title, body: answer.body }]]);
    } catch (err) {
      if (mine !== generation) return;
      error = { message: userFacingError(err), captured: captureError(err) };
    } finally {
      if (mine === generation) loading = false;
    }
  }

  const shown = $derived<MergeMessage>(edited ?? defaults.get(method) ?? { title: '', body: '' });

  function edit(next: Partial<MergeMessage>): void {
    if (!live || loading) return;
    edited = { ...shown, ...next };
  }

  function reset(): void {
    generation += 1;
    open = false;
    openedOn = null;
    edited = null;
    defaults = new Map();
    loading = false;
    error = null;
    notice = null;
  }

  // The one selection change: a pick, or a fallback (`was` no longer
  // offered), which keeps edited text and says so.
  function select(next: string, was?: string): void {
    if (next === selected) return;
    selected = next;
    notice = was !== undefined && edited !== null ? fallbackNotice(was, next) : null;
    if (live && edited === null) void read(next);
  }

  // Every snapshot re-resolves the selection against what it offers.
  function reconcile(methods: readonly string[] | null): void {
    if (methods === null) return;
    const offered = methods.length ? methods : ['merge'];
    if (selected === null) selected = offered[0];
    else if (!offered.includes(selected)) select(offered[0], selected);
  }
  const stopReconcile = $effect.root(() => {
    $effect(() => {
      const methods = host.methods;
      untrack(() => reconcile(methods));
    });
  });

  return {
    get method() { return method; },
    pick(next: string): void {
      select(next);
    },
    get open() { return live; },
    get notice() { return live ? notice : null; },
    get title() { return shown.title; },
    get body() { return shown.body; },
    get edited() { return edited !== null; },
    get loading() { return loading; },
    get error() { return error; },
    openEditor(): void {
      if (live) return;
      const ref = host.prRef;
      const key = host.identity;
      if (!ref || key === null) return;
      reset();
      open = true;
      openedOn = { ref, key };
      void read(method);
    },
    setTitle(text: string): void {
      edit({ title: text.replace(/[\r\n]+/g, ' ') });
    },
    setBody(text: string): void {
      edit({ body: text });
    },
    close(): void {
      reset();
    },
    get commitMessage(): MergeMessage | undefined {
      if (!live || edited === null || !methodHasCommitMessage(method)) return undefined;
      return { title: edited.title.trim(), body: edited.body };
    },
    discard(): void {
      reset();
      selected = null;
      reconcile(untrack(() => host.methods));
    },
    dispose(): void {
      stopReconcile();
      reset();
      selected = null;
    },
  };
}

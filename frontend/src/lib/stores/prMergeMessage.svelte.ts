// The Merge section's method and commit message editor: the method the
// reader picked in the Merge menu (it outlives the section's mount), and
// the forge's default message for it, read when the editor opens and
// when the method changes (never per tick), until the reader edits it.
// Only an edited message is sent, and only with the method it was
// written for; untouched, the forge's default applies as it does
// without the editor. A method that makes no commit (rebase,
// fast-forward) has no message. A failed default read shows its sentence
// in the editor and never stands in the way of a merge. The editor
// belongs to the PR it opened on: on another PR it reads closed and
// sends nothing, and `discard` drops it with the pick.
//
// A method the forge stops offering (a snapshot drops it) falls back to
// the project's default through the same path as a pick, keeping edited
// text, with a notice saying what happened: the message now goes with
// the new method's commit, or, for a method that makes none, the editor
// stays open with the text and the merge sends none. What the editor
// shows is what a merge sends.

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
  /** The method a merge uses: the reader's pick while `methods` offers
   * it, else the project's default (the first). */
  selected(methods: readonly string[]): string;
  /** The reader picked `method` in the Merge menu: the editor follows,
   * reading its default unless the text was edited. */
  pick(method: string): void;
  readonly open: boolean;
  /** The method the editor shows the message of. */
  readonly method: string;
  readonly title: string;
  readonly body: string;
  /** The reader changed the text; it is what a merge sends. */
  readonly edited: boolean;
  /** The default for the method is being read. */
  readonly loading: boolean;
  /** The default read failed; the editor starts empty. */
  readonly error: PRManageError | null;
  /** The method was no longer offered and the text moved to another. */
  readonly notice: string | null;
  openFor(method: string): void;
  setTitle(text: string): void;
  setBody(text: string): void;
  close(): void;
  /** What a merge with `method` sends as its commit message: the edited
   * text while the editor is open on this PR for that method, else
   * nothing. */
  messageFor(method: string): MergeMessage | undefined;
  /** The PR changed: the editor closes and the pick is dropped. */
  discard(): void;
  dispose(): void;
}

export function createPRMergeMessageState(host: PRMergeMessageHost): PRMergeMessageView {
  let open = $state(false);
  // The PR the editor opened on.
  let openedOn = $state<{ ref: PRRef; key: string } | null>(null);
  let method = $state('');
  let picked = $state<string | null>(null);
  let edited = $state<MergeMessage | null>(null);
  // The defaults read while this editor was open, per method.
  let defaults = $state.raw<ReadonlyMap<string, MergeMessage>>(new Map());
  let loading = $state(false);
  let error = $state<PRManageError | null>(null);
  let notice = $state<string | null>(null);
  let generation = 0;

  const live = $derived(open && openedOn !== null && openedOn.key === host.identity);

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

  // The one method-change path: a pick, or a fallback (`was` no longer
  // offered), which keeps edited text and says so.
  function follow(next: string, was?: string): void {
    if (!live || next === method) return;
    method = next;
    notice = was !== undefined && edited !== null ? fallbackNotice(was, next) : null;
    if (edited === null) void read(next);
  }

  // The snapshot's methods: a pick or the editor's method it no longer
  // offers falls back to the project's default.
  function reconcile(offered: readonly string[]): void {
    const fallback = offered[0];
    if (picked !== null && !offered.includes(picked)) picked = fallback;
    if (live && !offered.includes(method)) follow(picked ?? fallback, method);
  }
  const stopReconcile = $effect.root(() => {
    $effect(() => {
      const methods = host.methods;
      if (methods === null) return;
      const offered = methods.length ? methods : ['merge'];
      untrack(() => reconcile(offered));
    });
  });

  return {
    selected(methods: readonly string[]): string {
      return picked !== null && methods.includes(picked) ? picked : methods[0];
    },
    pick(next: string): void {
      picked = next;
      follow(next);
    },
    get open() { return live; },
    get notice() { return live ? notice : null; },
    get method() { return method; },
    get title() { return shown.title; },
    get body() { return shown.body; },
    get edited() { return edited !== null; },
    get loading() { return loading; },
    get error() { return error; },
    openFor(next: string): void {
      if (live) {
        follow(next);
        return;
      }
      const ref = host.prRef;
      const key = host.identity;
      if (!ref || key === null) return;
      reset();
      open = true;
      openedOn = { ref, key };
      method = next;
      void read(next);
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
    messageFor(forMethod: string): MergeMessage | undefined {
      if (!live || edited === null || forMethod !== method || !methodHasCommitMessage(forMethod)) return undefined;
      return { title: edited.title.trim(), body: edited.body };
    },
    discard(): void {
      reset();
      picked = null;
    },
    dispose(): void {
      stopReconcile();
      reset();
      picked = null;
    },
  };
}

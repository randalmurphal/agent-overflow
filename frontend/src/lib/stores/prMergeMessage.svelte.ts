// The Merge section's commit message editor: the forge's default for
// the chosen method, read when the editor opens and when the method
// changes (never per tick), until the reader edits it. Only an edited
// message is sent with Merge now or Auto-merge; untouched, the forge's
// default applies as it does without the editor. A method that makes no
// commit (rebase, fast-forward) has no message. A failed default read
// shows its sentence in the editor and never stands in the way of a
// merge.

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
  readonly backend: BackendKey;
}

export interface PRMergeMessageView {
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
  openFor(method: string): void;
  /** The Merge menu's method changed: read its default unless edited. */
  methodChanged(method: string): void;
  setTitle(text: string): void;
  setBody(text: string): void;
  close(): void;
  /** What a merge with `method` sends as its commit message: the edited
   * text while the editor is open, else nothing. */
  messageFor(method: string): MergeMessage | undefined;
  dispose(): void;
}

export function createPRMergeMessageState(host: PRMergeMessageHost): PRMergeMessageView {
  let open = $state(false);
  let method = $state('');
  let edited = $state<MergeMessage | null>(null);
  // The defaults read while this editor was open, per method.
  let defaults = $state.raw<ReadonlyMap<string, MergeMessage>>(new Map());
  let loading = $state(false);
  let error = $state<PRManageError | null>(null);
  let generation = 0;

  async function read(forMethod: string): Promise<void> {
    const ref = host.prRef;
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
    if (!open || loading) return;
    edited = { ...shown, ...next };
  }

  function reset(): void {
    generation += 1;
    open = false;
    edited = null;
    defaults = new Map();
    loading = false;
    error = null;
  }

  return {
    get open() { return open; },
    get method() { return method; },
    get title() { return shown.title; },
    get body() { return shown.body; },
    get edited() { return edited !== null; },
    get loading() { return loading; },
    get error() { return error; },
    openFor(next: string): void {
      if (open) {
        this.methodChanged(next);
        return;
      }
      reset();
      open = true;
      method = next;
      void read(next);
    },
    methodChanged(next: string): void {
      if (!open || next === method) return;
      method = next;
      if (edited === null) void read(next);
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
      if (!open || edited === null || !methodHasCommitMessage(forMethod)) return undefined;
      return { title: edited.title.trim(), body: edited.body };
    },
    dispose(): void {
      reset();
    },
  };
}

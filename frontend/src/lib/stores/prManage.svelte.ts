// One review's PR management (docs/specs/pull-requests.md, "Managing a
// PR"): the title and description editors, the Details section's open
// state and pickers, the title bar's state changes, re-request, dismiss
// and bypass, and the merge commit message editor.
//
// It follows the Merge section's contract. Nothing is optimistic: a
// write asks the forge, the backend has the PR's pump poll and answers
// with the pump's sequence after that poll, and the action reads as
// pending until the pane's applied sequence for the PR reaches it (`seq`
// 0, or one already applied, settles at once). The wait ends with the
// backend's launch, since a restarted backend numbers from one again,
// and with a failing pump, whose banner says why the forge's state is
// unknown. One action runs at a time, and none while the host withholds
// actions (a stale diff, a failing pump). An editor's text lives here,
// not in its component: the overview is a virtualized row and unmounts.
// An editor whose write is pending cannot be dismissed; a failed write
// keeps it open with the sentence on it.
//
// Every editor, the picker and the merge message belong to the PR they
// opened on (its computer and PR) and write only to it. When the pane's
// PR becomes another, all of it is dropped: open editors and the picker
// close without writing, the merge message and its method go, and a
// late answer for the PR that was is ignored. The pane closing applies
// an open picker's change (its failure is a toast, its row being gone). A failure shows on the surface
// that caused it where that surface stays: a picker's or re-request's on
// its Details row, Dismiss's and Bypass's on the changes-requested
// blocker row. The state menu's items toast, since their menu is gone.

import { untrack } from 'svelte';
import {
  BypassPRRequestedChanges,
  DismissPRReview,
  EditPR,
  RerequestPRReview,
  SetPRAssignees,
  SetPRLabels,
  SetPRReviewers,
  SetPRState,
} from './bindings';
import { captureError, type CapturedError } from './errorReports.svelte';
import { createPRMergeMessageState, type PRMergeMessageView } from './prMergeMessage.svelte';
import { createPRPickerState, type PRManageTarget, type PRPickerView } from './prManagePicker.svelte';
import { addErrorToast, addToast } from './toast.svelte';
import { withBackendTarget } from '../transport/backends';
import type { BackendKey } from '../transport/backendKey';
import type { PRPerson } from '../types/models';
import { labelChange, peopleChange, PR_STATE_LABEL, wirePerson, type PRManageRow, type PRStateChange } from '../utils/prManage';
import { prReferenceWire, type PRRef } from '../utils/prReference';
import { userFacingError } from '../utils/userFacingError';

/** What the review pane lends the management state. */
export interface PRManageHost {
  readonly prRef: PRRef | null;
  /** The PR's computer-scoped key whatever the scope (computerPRKey of
   * prRef), or null: the identity the management state belongs to. */
  readonly identity: string | null;
  /** The PR's computer-scoped key (computerPRKey), whose applied
   * sequence settles a pending action; null outside the PR scope. */
  readonly entityKey: string | null;
  /** The PR's applied sequence (peekPRSeq of entityKey). */
  readonly appliedSeq: number;
  readonly backend: BackendKey;
  /** The backend's launch id (its hello's launchId). */
  readonly launch: string;
  /** Why actions are withheld (the merge actions' reason), or null. */
  readonly withheldReason: string | null;
  /** The PR's pump is failing. */
  readonly updateFailing: boolean;
  /** Re-read the PR after a failure, as the merge actions do. */
  refresh(): void;
}

export type PRManageActionKind =
  | 'edit_title' | 'edit_description'
  | PRManageRow
  | PRStateChange
  | 'rerequest' | 'dismiss' | 'bypass';

export const PR_MANAGE_ACTION_LABEL: Readonly<Record<PRManageActionKind, string>> = {
  edit_title: 'Editing the title',
  edit_description: 'Editing the description',
  reviewers: 'Changing reviewers',
  assignees: 'Changing assignees',
  labels: 'Changing labels',
  close: PR_STATE_LABEL.close,
  reopen: PR_STATE_LABEL.reopen,
  draft: PR_STATE_LABEL.draft,
  rerequest: 'Re-requesting the review',
  dismiss: 'Dismissing the review',
  bypass: 'Bypassing the requested changes',
};

/** A pending action: its kind and what it names (a reviewer's login, a
 * review id), '' when nothing. */
export interface PRManagePending {
  readonly kind: PRManageActionKind;
  readonly target: string;
}

/** A failure shown on the surface that caused it. */
export interface PRManageError {
  readonly message: string;
  readonly captured: CapturedError;
}

/** Hover text of a control that cannot be dismissed while it saves. */
export const SAVING_TITLE = 'Still saving this change.';

export interface PRManageView {
  /** The action in flight or waiting for the pump's answer. */
  readonly pending: PRManagePending | null;
  /** Why no action may run now, for hover text, or null. */
  readonly blockedReason: string | null;

  // The title editor.
  readonly titleDraft: string | null;
  readonly titleError: PRManageError | null;
  readonly titleSaving: boolean;
  openTitle(current: string): void;
  setTitleDraft(text: string): void;
  cancelTitle(): void;
  saveTitle(): Promise<void>;

  // The description editor.
  readonly descriptionDraft: string | null;
  readonly descriptionPreview: boolean;
  readonly descriptionError: PRManageError | null;
  readonly descriptionSaving: boolean;
  openDescription(current: string): void;
  setDescriptionDraft(text: string): void;
  setDescriptionPreview(preview: boolean): void;
  cancelDescription(): void;
  saveDescription(): Promise<void>;

  // The Details section.
  /** The Details section is open; collapsed until the reader opens it,
   * then remembered as the other overview sections are. */
  readonly detailsOpen: boolean;
  setDetailsOpen(open: boolean): void;
  readonly picker: PRPickerView;
  /** A row's failed change or re-request (reviewers), shown on the row
   * until its picker opens again or the row's next write starts. */
  rowError(row: PRManageRow): PRManageError | null;

  // The title bar's state changes and the reviews.
  setState(state: PRStateChange): Promise<void>;
  rerequest(reviewer: PRPerson): Promise<void>;
  /** The open Dismiss reason popover: the review it dismisses and its text. */
  readonly dismissDraft: { readonly reviewId: string; readonly text: string } | null;
  /** The changes-requested blocker row's failed Dismiss or Bypass, until
   * the next Dismiss opens or Bypass starts. */
  readonly changeRequestError: PRManageError | null;
  /** Opens the reason popover; after a failed dismissal of the same
   * review it opens with the reason that failed. */
  openDismiss(reviewId: string): void;
  setDismissText(text: string): void;
  cancelDismiss(): void;
  dismiss(): Promise<void>;
  bypass(): Promise<void>;

  /** The Merge section's commit message editor. */
  readonly mergeMessage: PRMergeMessageView;

  dispose(): void;
}

interface Await extends PRManagePending {
  readonly key: string;
  readonly seq: number;
  readonly launch: string;
}

// A write's outcome; `blocked` carries why it did not start.
type Outcome = { ok: true } | { ok: false; err: unknown } | { ok: false; blocked: string };

export function createPRManageState(host: PRManageHost): PRManageView {
  let running = $state<PRManagePending | null>(null);
  let awaiting = $state<Await | null>(null);
  let disposed = false;

  const pending = $derived.by<PRManagePending | null>(() => {
    if (running) return running;
    const wait = awaiting;
    if (wait === null || wait.key !== host.entityKey || wait.launch !== host.launch || host.updateFailing) return null;
    return host.appliedSeq < wait.seq ? wait : null;
  });
  const blockedReason = $derived(
    host.withheldReason ?? (pending ? `${PR_MANAGE_ACTION_LABEL[pending.kind]}…` : null),
  );

  // The PR on screen, to capture when an editor or picker opens.
  function currentTarget(): PRManageTarget | null {
    const ref = host.prRef;
    const key = host.identity;
    return ref && key !== null ? { ref, key } : null;
  }

  // An answer about `on` still belongs on screen.
  function onScreen(on: PRManageTarget): boolean {
    return !disposed && on.key === host.identity;
  }

  // Runs one write to `on`; answers how it went. The caller shows a
  // failure on its own surface, and drops an answer for a PR no longer
  // on screen.
  async function run(kind: PRManageActionKind, target: string, on: PRManageTarget, call: (ref: PRRef) => Promise<{ seq: number }>): Promise<Outcome> {
    const blocked = untrack(() => blockedReason);
    if (blocked !== null) return { ok: false, blocked };
    const launch = host.launch;
    running = { kind, target };
    awaiting = null;
    let result: { seq: number };
    try {
      result = await withBackendTarget(host.backend, () => call(on.ref));
    } catch (err) {
      if (disposed) return { ok: false, err };
      running = null;
      if (on.key === host.identity) host.refresh();
      return { ok: false, err };
    }
    if (disposed) return { ok: true };
    running = null;
    if (host.launch === launch && host.entityKey === on.key && untrack(() => host.appliedSeq) < result.seq) awaiting = { kind, target, key: on.key, seq: result.seq, launch };
    return { ok: true };
  }

  // A write that did not start shows why as it is (it is hover text
  // already); a failed one shows the backend's sentence.
  function failure(outcome: Exclude<Outcome, { ok: true }>): PRManageError {
    if ('blocked' in outcome) {
      const err = new Error(outcome.blocked);
      return { message: outcome.blocked, captured: captureError(err) };
    }
    return { message: userFacingError(outcome.err), captured: captureError(outcome.err) };
  }

  function toastFailure(kind: PRManageActionKind, outcome: Exclude<Outcome, { ok: true }>): void {
    if ('blocked' in outcome) {
      addToast('error', `${PR_MANAGE_ACTION_LABEL[kind]} did not start: ${outcome.blocked}`);
      return;
    }
    addErrorToast(`${PR_MANAGE_ACTION_LABEL[kind]} failed: ${userFacingError(outcome.err)}`, outcome.err);
  }

  // An editor closes once its write's answer is on screen: `submitted`
  // marks a write that succeeded, and the editor reads closed as soon as
  // its action is no longer pending.
  // An editor shows only on the PR it opened on.
  let title = $state<{ text: string; submitted: boolean; on: PRManageTarget } | null>(null);
  let titleError = $state<PRManageError | null>(null);
  const titleSaving = $derived(pending?.kind === 'edit_title');
  const titleDraft = $derived(title && title.on.key === host.identity && !(title.submitted && !titleSaving) ? title.text : null);

  let description = $state<{ text: string; expected: string; preview: boolean; submitted: boolean; on: PRManageTarget } | null>(null);
  let descriptionError = $state<PRManageError | null>(null);
  const descriptionSaving = $derived(pending?.kind === 'edit_description');
  const descriptionOpen = $derived(description !== null && description.on.key === host.identity && !(description.submitted && !descriptionSaving));

  let detailsOpen = $state(false);
  let rowErrors = $state<Partial<Record<PRManageRow, PRManageError>>>({});
  function setRowError(row: PRManageRow, error: PRManageError | null): void {
    const next = { ...rowErrors };
    if (error) next[row] = error;
    else delete next[row];
    rowErrors = next;
  }

  let dismissState = $state<{ reviewId: string; text: string; submitted: boolean; on: PRManageTarget } | null>(null);
  // The reason of a dismissal that failed, offered again on its reopen.
  let failedDismiss: { reviewId: string; text: string } | null = null;
  let changeRequestError = $state<PRManageError | null>(null);
  const dismissSaving = $derived(pending?.kind === 'dismiss');
  const dismissDraft = $derived(dismissState && dismissState.on.key === host.identity && !(dismissState.submitted && !dismissSaving)
    ? { reviewId: dismissState.reviewId, text: dismissState.text }
    : null);

  const picker = createPRPickerState({
    get target() { return currentTarget(); },
    get identity() { return host.identity; },
    get backend() { return host.backend; },
    async apply(row, change, on, closing) {
      const wire = row === 'labels' ? labelChange(change.labels) : peopleChange(change.people);
      const outcome = await run(row, '', on, (ref) => {
        const pr = prReferenceWire(ref);
        if (row === 'labels') return SetPRLabels(pr, wire as ReturnType<typeof labelChange>);
        return row === 'reviewers' ? SetPRReviewers(pr, wire as ReturnType<typeof peopleChange>) : SetPRAssignees(pr, wire as ReturnType<typeof peopleChange>);
      });
      // Applied as the pane closed: no row is left to show a failure.
      if (closing) {
        if (!outcome.ok) toastFailure(row, outcome);
        return;
      }
      if (!onScreen(on)) return;
      // The sentence as the backend gave it, on the row; a partial change
      // says so, and the row shows the next snapshot either way.
      setRowError(row, outcome.ok ? null : failure(outcome));
    },
    opened(row) {
      setRowError(row, null);
    },
  });

  const mergeMessage = createPRMergeMessageState({
    get prRef() { return host.prRef; },
    get identity() { return host.identity; },
    get backend() { return host.backend; },
  });

  // The PR on screen became another: drop everything that was its.
  function discardAll(): void {
    title = null;
    titleError = null;
    description = null;
    descriptionError = null;
    dismissState = null;
    failedDismiss = null;
    changeRequestError = null;
    rowErrors = {};
    picker.discard();
    mergeMessage.discard();
  }
  let identity: string | null = untrack(() => host.identity);
  const stopIdentityWatch = $effect.root(() => {
    $effect(() => {
      const next = host.identity;
      // A moment without a PR (git status settling) is not another PR.
      if (next === null || next === identity) return;
      const was = identity;
      identity = next;
      if (was !== null) untrack(discardAll);
    });
  });

  return {
    get pending() { return pending; },
    get blockedReason() { return blockedReason; },

    get titleDraft() { return titleDraft; },
    get titleError() { return titleError; },
    get titleSaving() { return titleSaving; },
    openTitle(current: string): void {
      const on = currentTarget();
      if (titleDraft !== null || !on) return;
      title = { text: current, submitted: false, on };
      titleError = null;
    },
    setTitleDraft(text: string): void {
      if (title && !titleSaving) title = { ...title, text };
    },
    cancelTitle(): void {
      if (titleSaving) return;
      title = null;
      titleError = null;
    },
    async saveTitle(): Promise<void> {
      const draft = title;
      if (!draft || titleDraft === null || titleSaving) return;
      const text = draft.text.trim();
      if (text === '') {
        titleError = failure({ ok: false, blocked: 'The title cannot be empty.' });
        return;
      }
      titleError = null;
      const outcome = await run('edit_title', '', draft.on, (ref) => EditPR(prReferenceWire(ref), { title: text }));
      if (!onScreen(draft.on) || title !== draft) return;
      if (outcome.ok) title = { ...draft, submitted: true };
      else titleError = failure(outcome);
    },

    get descriptionDraft() { return descriptionOpen ? (description?.text ?? '') : null; },
    get descriptionPreview() { return descriptionOpen && !!description?.preview; },
    get descriptionError() { return descriptionError; },
    get descriptionSaving() { return descriptionSaving; },
    openDescription(current: string): void {
      const on = currentTarget();
      if (descriptionOpen || !on) return;
      description = { text: current, expected: current, preview: false, submitted: false, on };
      descriptionError = null;
    },
    setDescriptionDraft(text: string): void {
      if (description && !descriptionSaving) description = { ...description, text };
    },
    setDescriptionPreview(preview: boolean): void {
      if (description) description = { ...description, preview };
    },
    cancelDescription(): void {
      if (descriptionSaving) return;
      description = null;
      descriptionError = null;
    },
    async saveDescription(): Promise<void> {
      const draft = description;
      if (!draft || !descriptionOpen || descriptionSaving) return;
      descriptionError = null;
      // The description the editor opened with is the precondition: a
      // description changed since is refused, and the editor stays open
      // with the backend's sentence.
      const outcome = await run('edit_description', '', draft.on, (ref) => EditPR(prReferenceWire(ref), { description: draft.text, expectedDescription: draft.expected }));
      if (!onScreen(draft.on) || description === null || description.on !== draft.on || description.expected !== draft.expected) return;
      if (outcome.ok) description = { ...description, submitted: true };
      else descriptionError = failure(outcome);
    },

    get detailsOpen() { return detailsOpen; },
    setDetailsOpen(open: boolean): void { detailsOpen = open; },
    picker,
    rowError(row: PRManageRow): PRManageError | null { return rowErrors[row] ?? null; },

    async setState(state: PRStateChange): Promise<void> {
      const on = currentTarget();
      if (!on) return;
      const outcome = await run(state, '', on, (ref) => SetPRState(prReferenceWire(ref), state));
      if (onScreen(on) && !outcome.ok) toastFailure(state, outcome);
    },
    async rerequest(reviewer: PRPerson): Promise<void> {
      const on = currentTarget();
      if (!on) return;
      const person = wirePerson(reviewer);
      setRowError('reviewers', null);
      const outcome = await run('rerequest', reviewer.login, on, (ref) => RerequestPRReview(prReferenceWire(ref), person));
      if (onScreen(on) && !outcome.ok) setRowError('reviewers', failure(outcome));
    },
    get dismissDraft() { return dismissDraft; },
    get changeRequestError() { return changeRequestError; },
    openDismiss(reviewId: string): void {
      const on = currentTarget();
      if (dismissSaving || !on) return;
      const text = failedDismiss?.reviewId === reviewId ? failedDismiss.text : '';
      failedDismiss = null;
      dismissState = { reviewId, text, submitted: false, on };
      changeRequestError = null;
    },
    setDismissText(text: string): void {
      if (dismissState && !dismissSaving) dismissState = { ...dismissState, text };
    },
    cancelDismiss(): void {
      if (dismissSaving) return;
      dismissState = null;
    },
    async dismiss(): Promise<void> {
      const draft = dismissState;
      if (!draft || dismissDraft === null || dismissSaving) return;
      const message = draft.text.trim();
      if (message === '') return;
      changeRequestError = null;
      const outcome = await run('dismiss', draft.reviewId, draft.on, (ref) => DismissPRReview(prReferenceWire(ref), draft.reviewId, message));
      if (!onScreen(draft.on) || dismissState !== draft) return;
      if (outcome.ok) {
        dismissState = { ...draft, submitted: true };
        return;
      }
      // The popover closes and the sentence goes on the blocker row; the
      // reason waits for the next Dismiss of that review.
      dismissState = null;
      failedDismiss = { reviewId: draft.reviewId, text: draft.text };
      changeRequestError = failure(outcome);
    },
    async bypass(): Promise<void> {
      const on = currentTarget();
      if (!on) return;
      changeRequestError = null;
      const outcome = await run('bypass', '', on, (ref) => BypassPRRequestedChanges(prReferenceWire(ref)));
      if (onScreen(on) && !outcome.ok) changeRequestError = failure(outcome);
    },

    mergeMessage,

    dispose(): void {
      stopIdentityWatch();
      // The open picker's change is written before the state goes.
      picker.dispose();
      disposed = true;
      mergeMessage.dispose();
    },
  };
}

// The PR management state (prManage.svelte.ts, prManagePicker.svelte.ts,
// prMergeMessage.svelte.ts): an action is pending until the PR's applied
// sequence reaches the one it answered with, one runs at a time and none
// while withheld; an editor cannot be dismissed while it saves, closes
// once the answer is on screen and stays open with the sentence on a
// failure; a picker's adds and removes are one write, none when nothing
// changed, an older search's results go the moment the query changes,
// and labels are one option per id through a rename; the merge commit
// message is sent only when edited, and only for its method; everything
// belongs to the PR it opened on and is dropped when the PR changes; the
// pane closing applies an open picker's change.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { flushSync } from 'svelte';
import { createPRManageState, type PRManageHost, type PRManageView } from './prManage.svelte';
import { PICKER_SEARCH_DEBOUNCE_MS } from './prManagePicker.svelte';
import { getToasts, resetToastsForTest } from './toast.svelte';
import { HOME_BACKEND } from '../transport/backendKey';
import { TransportError } from '../transport/wsClient';
import type { PRCandidates } from '../types/models';
import { setBindingMock } from '../../test/mocks/bindings-app';

const REF = { forge: 'github' as const, host: 'github.com', namespace: 'owner', repo: 'repo', number: 5 };
const REF_B = { ...REF, number: 6 };
const KEY = 'home:github:owner/repo:5';
const KEY_B = 'home:github:owner/repo:6';

function makeHost() {
  const host = $state({
    prRef: REF as typeof REF | null,
    identity: KEY as string | null,
    entityKey: KEY as string | null,
    appliedSeq: 1,
    backend: HOME_BACKEND,
    launch: 'L1',
    withheldReason: null as string | null,
    updateFailing: false,
    refreshes: 0,
  });
  const lent: PRManageHost = {
    get prRef() { return host.prRef; },
    get identity() { return host.identity; },
    get entityKey() { return host.entityKey; },
    get appliedSeq() { return host.appliedSeq; },
    backend: HOME_BACKEND,
    get launch() { return host.launch; },
    get withheldReason() { return host.withheldReason; },
    get updateFailing() { return host.updateFailing; },
    refresh: () => { host.refreshes += 1; },
  };
  return { host, lent };
}

async function settle(): Promise<void> {
  for (let i = 0; i < 5; i += 1) await Promise.resolve();
  flushSync();
}

function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (err: unknown) => void;
  const promise = new Promise<T>((res, rej) => { resolve = res; reject = rej; });
  return { promise, resolve, reject };
}

let cleanup: (() => void)[] = [];
let view: PRManageView;
let host: ReturnType<typeof makeHost>['host'];

beforeEach(() => {
  resetToastsForTest();
  const made = makeHost();
  host = made.host;
  cleanup.push($effect.root(() => {
    view = createPRManageState(made.lent);
  }));
});

afterEach(() => {
  view.dispose();
  for (const each of cleanup) each();
  cleanup = [];
  vi.useRealTimers();
});

describe('pending', () => {
  it('holds an action until the applied sequence reaches its answer', async () => {
    setBindingMock('SetPRState', async () => ({ seq: 3 }));
    await view.setState('close');
    expect(view.pending).toEqual(expect.objectContaining({ kind: 'close' }));
    host.appliedSeq = 2;
    expect(view.pending).not.toBeNull();
    host.appliedSeq = 3;
    expect(view.pending).toBeNull();
  });

  it('settles at once on seq 0 or one already applied', async () => {
    setBindingMock('SetPRState', async () => ({ seq: 0 }));
    await view.setState('close');
    expect(view.pending).toBeNull();
    setBindingMock('SetPRState', async () => ({ seq: 1 }));
    await view.setState('reopen');
    expect(view.pending).toBeNull();
  });

  it('gives the wait up with a new launch or a failing pump', async () => {
    setBindingMock('SetPRState', async () => ({ seq: 9 }));
    await view.setState('close');
    host.launch = 'L2';
    expect(view.pending).toBeNull();
    host.launch = 'L1';
    expect(view.pending).not.toBeNull();
    host.updateFailing = true;
    expect(view.pending).toBeNull();
  });

  it('runs one action at a time and none while withheld', async () => {
    const state = setBindingMock('SetPRState', async () => ({ seq: 4 }));
    const bypass = setBindingMock('BypassPRRequestedChanges', async () => ({ seq: 5 }));
    await view.setState('close');
    expect(view.blockedReason).toBe('Close…');
    await view.bypass();
    expect(bypass).not.toHaveBeenCalled();
    expect(view.changeRequestError?.message).toBe('Close…');
    host.appliedSeq = 4;
    host.withheldReason = 'The diff is still loading.';
    await view.setState('reopen');
    expect(state).toHaveBeenCalledTimes(1);
    expect(view.blockedReason).toBe('The diff is still loading.');
  });

  it('toasts a failed state change with its sentence and re-reads the PR', async () => {
    setBindingMock('SetPRState', async () => { throw new TransportError('forge_forbidden', 'No access to owner/repo with this login.'); });
    await view.setState('close');
    expect(view.pending).toBeNull();
    expect(getToasts().at(-1)?.message).toBe('Close failed: No access to owner/repo with this login.');
    expect(host.refreshes).toBe(1);
  });
});

describe('the title editor', () => {
  it('cannot be dismissed while saving and closes once the answer is on screen', async () => {
    const call = deferred<{ seq: number }>();
    const edit = setBindingMock('EditPR', () => call.promise);
    view.openTitle('Old');
    view.setTitleDraft('  New title ');
    const saving = view.saveTitle();
    flushSync();
    expect(view.titleSaving).toBe(true);
    view.cancelTitle();
    view.setTitleDraft('changed while saving');
    expect(view.titleDraft).toBe('  New title ');
    call.resolve({ seq: 2 });
    await saving;
    expect(edit).toHaveBeenCalledWith(expect.anything(), { title: 'New title' });
    expect(view.titleDraft).toBe('  New title ');
    host.appliedSeq = 2;
    expect(view.titleDraft).toBeNull();
  });

  it('stays open with the sentence on a failure', async () => {
    setBindingMock('EditPR', async () => { throw new TransportError('forge_forbidden', 'GitHub blocks this because the login cannot update the pull request.'); });
    view.openTitle('Old');
    view.setTitleDraft('New');
    await view.saveTitle();
    expect(view.titleDraft).toBe('New');
    expect(view.titleError?.message).toBe('GitHub blocks this because the login cannot update the pull request.');
    expect(view.titleSaving).toBe(false);
  });
});

describe('the description editor', () => {
  it('names the description it opened with, and keeps the editor open when the forge refuses it', async () => {
    const sentence = 'The description changed since you opened it. Copy your text, reload the description and edit again.';
    const edit = setBindingMock('EditPR', async () => { throw new TransportError('forge_refused', sentence); });
    view.openDescription('Opened');
    view.setDescriptionDraft('Mine');
    await view.saveDescription();
    expect(edit).toHaveBeenCalledWith(expect.anything(), { description: 'Mine', expectedDescription: 'Opened' });
    expect(view.descriptionDraft).toBe('Mine');
    expect(view.descriptionError?.message).toBe(sentence);
  });

  it('keeps the preview choice and the text in the state, and discards on cancel', () => {
    view.openDescription('Body');
    view.setDescriptionPreview(true);
    expect(view.descriptionPreview).toBe(true);
    view.cancelDescription();
    expect(view.descriptionDraft).toBeNull();
    view.openDescription('Body');
    expect(view.descriptionPreview).toBe(false);
  });
});

describe('the dismiss reason', () => {
  it('needs a reason, cannot be cancelled while pending, and closes on the answer', async () => {
    const dismiss = setBindingMock('DismissPRReview', async () => ({ seq: 3 }));
    view.openDismiss('R1');
    await view.dismiss();
    expect(dismiss).not.toHaveBeenCalled();
    view.setDismissText(' Addressed. ');
    await view.dismiss();
    expect(dismiss).toHaveBeenCalledWith(expect.anything(), 'R1', 'Addressed.');
    view.cancelDismiss();
    expect(view.dismissDraft).not.toBeNull();
    host.appliedSeq = 3;
    expect(view.dismissDraft).toBeNull();
  });
});

describe("where a review action's failure shows", () => {
  const REFUSED = 'GitLab refused this because the login cannot approve merge requests here.';

  it('puts a failed dismissal on the blocker row, closes the reason and offers it again on that review only', async () => {
    const dismiss = setBindingMock('DismissPRReview', async () => { throw new TransportError('forge_forbidden', REFUSED); });
    view.openDismiss('R1');
    view.setDismissText('Addressed');
    await view.dismiss();
    expect(dismiss).toHaveBeenCalledTimes(1);
    expect(view.dismissDraft).toBeNull();
    expect(view.changeRequestError?.message).toBe(REFUSED);
    expect(getToasts()).toHaveLength(0);
    view.openDismiss('R2');
    expect(view.dismissDraft).toEqual({ reviewId: 'R2', text: '' });
    expect(view.changeRequestError).toBeNull();
    view.cancelDismiss();
    view.openDismiss('R1');
    expect(view.dismissDraft?.text).toBe('');
  });

  it("reopens the failed review's Dismiss with its reason", async () => {
    setBindingMock('DismissPRReview', async () => { throw new TransportError('forge_forbidden', REFUSED); });
    view.openDismiss('R1');
    view.setDismissText('Addressed');
    await view.dismiss();
    view.openDismiss('R1');
    expect(view.dismissDraft).toEqual({ reviewId: 'R1', text: 'Addressed' });
  });

  it('puts a failed Bypass on the blocker row until the next one starts', async () => {
    let fail = true;
    setBindingMock('BypassPRRequestedChanges', async () => {
      if (fail) throw new TransportError('forge_forbidden', REFUSED);
      return { seq: 0 };
    });
    await view.bypass();
    expect(view.changeRequestError?.message).toBe(REFUSED);
    expect(getToasts()).toHaveLength(0);
    fail = false;
    await view.bypass();
    expect(view.changeRequestError).toBeNull();
  });

  it('puts a failed re-request on the Reviewers row, cleared by the next one or the picker', async () => {
    let fail = true;
    setBindingMock('RerequestPRReview', async () => {
      if (fail) throw new TransportError('forge_forbidden', REFUSED);
      return { seq: 0 };
    });
    setBindingMock('SearchPRCandidates', async () => ({ people: [], labels: [], more: false }));
    await view.rerequest({ login: 'kchen' });
    expect(view.rowError('reviewers')?.message).toBe(REFUSED);
    expect(view.rowError('assignees')).toBeNull();
    expect(getToasts()).toHaveLength(0);
    fail = false;
    await view.rerequest({ login: 'kchen' });
    expect(view.rowError('reviewers')).toBeNull();
    fail = true;
    await view.rerequest({ login: 'kchen' });
    view.picker.open('reviewers', { people: [] });
    expect(view.rowError('reviewers')).toBeNull();
  });
});

describe('the PR on screen changing', () => {
  async function switchPR(): Promise<void> {
    host.prRef = REF_B;
    host.identity = KEY_B;
    host.entityKey = KEY_B;
    await settle();
  }

  it('closes the title and description editors without writing, and for good', async () => {
    const edit = setBindingMock('EditPR', async () => ({ seq: 0 }));
    view.openTitle('A title');
    view.setTitleDraft('Meant for A');
    view.openDescription('A body');
    view.setDescriptionDraft('Meant for A');
    await switchPR();
    expect(view.titleDraft).toBeNull();
    expect(view.descriptionDraft).toBeNull();
    await view.saveTitle();
    await view.saveDescription();
    expect(edit).not.toHaveBeenCalled();
    // Back on A: what was typed for it is gone too.
    host.prRef = REF;
    host.identity = KEY;
    host.entityKey = KEY;
    await settle();
    expect(view.titleDraft).toBeNull();
    expect(view.descriptionDraft).toBeNull();
  });

  it('discards a picker with changes, writing nothing', async () => {
    setBindingMock('SearchPRCandidates', async () => ({ people: [{ login: 'jdoe' }], labels: [], more: false }));
    const set = setBindingMock('SetPRReviewers', async () => ({ seq: 0 }));
    view.picker.open('reviewers', { people: [] });
    await settle();
    view.picker.toggle('u:jdoe');
    await switchPR();
    expect(view.picker.row).toBeNull();
    await view.picker.close();
    view.dispose();
    await settle();
    expect(set).not.toHaveBeenCalled();
  });

  it('drops an edited merge message and its method', async () => {
    setBindingMock('GetPRMergeMessage', async () => ({ title: 'Default', body: '' }));
    const message = view.mergeMessage;
    message.pick('squash');
    message.openFor('squash');
    await settle();
    message.setTitle('Meant for A');
    await switchPR();
    expect(message.open).toBe(false);
    expect(message.messageFor('squash')).toBeUndefined();
    expect(message.selected(['merge', 'squash'])).toBe('merge');
  });

  it("hides what was A's at once, before the reset runs", async () => {
    setBindingMock('SearchPRCandidates', async () => ({ people: [{ login: 'jdoe' }], labels: [], more: false }));
    setBindingMock('GetPRMergeMessage', async () => ({ title: 'Default', body: '' }));
    view.openTitle('A title');
    view.picker.open('reviewers', { people: [] });
    view.mergeMessage.openFor('merge');
    await settle();
    view.mergeMessage.setTitle('Meant for A');
    // No flush: the identity watch has not run yet.
    host.prRef = REF_B;
    host.identity = KEY_B;
    expect(view.titleDraft).toBeNull();
    expect(view.picker.row).toBeNull();
    expect(view.mergeMessage.open).toBe(false);
    expect(view.mergeMessage.messageFor('merge')).toBeUndefined();
  });

  it('ignores a late answer for the PR that was', async () => {
    const call = deferred<{ seq: number }>();
    setBindingMock('SearchPRCandidates', async () => ({ people: [{ login: 'jdoe' }], labels: [], more: false }));
    setBindingMock('SetPRReviewers', () => call.promise);
    view.picker.open('reviewers', { people: [] });
    await settle();
    view.picker.toggle('u:jdoe');
    const closing = view.picker.close();
    await switchPR();
    call.reject(new TransportError('forge_refused', 'GitHub refused the reviewers.'));
    await closing;
    expect(view.rowError('reviewers')).toBeNull();
    expect(host.refreshes).toBe(0);
  });

  it('writes only to the PR an editor opened on', async () => {
    const call = deferred<{ seq: number }>();
    const edit = setBindingMock('EditPR', () => call.promise);
    view.openTitle('Old');
    view.setTitleDraft('New');
    const saving = view.saveTitle();
    host.prRef = REF_B;
    call.resolve({ seq: 0 });
    await saving;
    expect(edit.mock.calls[0][0]).toEqual(expect.objectContaining({ Number: 5 }));
  });
});

describe('the pane closing', () => {
  it("applies an open picker's change to its PR, and toasts a failure", async () => {
    setBindingMock('SearchPRCandidates', async () => ({ people: [{ login: 'jdoe' }], labels: [], more: false }));
    const set = setBindingMock('SetPRReviewers', async () => { throw new TransportError('forge_refused', 'GitHub refused the reviewers.'); });
    view.picker.open('reviewers', { people: [] });
    await settle();
    view.picker.toggle('u:jdoe');
    view.dispose();
    await settle();
    expect(set).toHaveBeenCalledWith(expect.objectContaining({ Number: 5 }), { add: [{ login: 'jdoe' }], remove: [] });
    expect(getToasts().at(-1)?.message).toBe('Changing reviewers failed: GitHub refused the reviewers.');
  });
});

describe('the picker', () => {
  const ALEE = { login: 'alee', name: 'Al Lee', state: 'PENDING' };
  const KCHEN = { login: 'kchen', state: 'CHANGES_REQUESTED' };
  const JDOE = { login: 'jdoe', name: 'Jane Doe' };

  it('applies its adds and removes as one write on close', async () => {
    setBindingMock('SearchPRCandidates', async (): Promise<PRCandidates> => ({ people: [JDOE], labels: [], more: false }));
    const set = setBindingMock('SetPRReviewers', async () => ({ seq: 2 }));
    view.picker.open('reviewers', { people: [ALEE, KCHEN], locked: ['u:kchen'] });
    await settle();
    view.picker.toggle('u:jdoe');
    view.picker.toggle('u:alee');
    view.picker.toggle('u:kchen');
    await view.picker.close();
    expect(set).toHaveBeenCalledTimes(1);
    expect(set).toHaveBeenCalledWith(expect.anything(), { add: [JDOE], remove: [{ login: 'alee', name: 'Al Lee' }] });
    expect(view.picker.row).toBeNull();
    expect(view.pending?.kind).toBe('reviewers');
  });

  it('writes nothing when nothing changed', async () => {
    setBindingMock('SearchPRCandidates', async () => ({ people: [JDOE], labels: [], more: false }));
    const set = setBindingMock('SetPRAssignees', async () => ({ seq: 2 }));
    view.picker.open('assignees', { people: [ALEE] });
    await settle();
    view.picker.toggle('u:jdoe');
    view.picker.toggle('u:jdoe');
    await view.picker.close();
    expect(set).not.toHaveBeenCalled();
  });

  it('drops an older search answer that lands after a newer one', async () => {
    vi.useFakeTimers();
    const answers: Record<string, ReturnType<typeof deferred<PRCandidates>>> = { '': deferred(), ja: deferred(), jan: deferred() };
    const search = setBindingMock('SearchPRCandidates', (_pr: unknown, _kind: string, query: string) => answers[query].promise);
    view.picker.open('assignees', {});
    view.picker.setQuery('ja');
    vi.advanceTimersByTime(PICKER_SEARCH_DEBOUNCE_MS - 1);
    view.picker.setQuery('jan');
    vi.advanceTimersByTime(PICKER_SEARCH_DEBOUNCE_MS);
    // Debounced: the first keystroke never searched.
    expect(search.mock.calls.map((callArgs) => callArgs[2])).toEqual(['', 'jan']);
    answers.jan.resolve({ people: [JDOE], labels: [], more: true });
    await settle();
    answers[''].resolve({ people: [ALEE], labels: [], more: false });
    await settle();
    expect(view.picker.options.map((option) => option.key)).toEqual(['u:jdoe']);
    expect(view.picker.more).toBe(true);
    expect(view.picker.loading).toBe(false);
  });

  it('drops the old results the moment the query changes, keeping what is checked', async () => {
    vi.useFakeTimers();
    const answers: Record<string, ReturnType<typeof deferred<PRCandidates>>> = { '': deferred(), ja: deferred() };
    setBindingMock('SearchPRCandidates', (_pr: unknown, _kind: string, query: string) => answers[query].promise);
    view.picker.open('assignees', { people: [ALEE] });
    answers[''].resolve({ people: [ALEE, KCHEN], labels: [], more: false });
    await settle();
    expect(view.picker.options.map((option) => option.key)).toEqual(['u:alee', 'u:kchen']);
    view.picker.setQuery('ja');
    // Inside the debounce: the old results are gone and cannot be picked.
    expect(view.picker.loading).toBe(true);
    expect(view.picker.options.map((option) => option.key)).toEqual(['u:alee']);
    view.picker.toggle('u:kchen');
    expect(view.picker.options.find((option) => option.key === 'u:kchen')).toBeUndefined();
    vi.advanceTimersByTime(PICKER_SEARCH_DEBOUNCE_MS);
    answers.ja.resolve({ people: [JDOE], labels: [], more: false });
    await settle();
    expect(view.picker.options.map((option) => [option.key, option.checked])).toEqual([['u:jdoe', false]]);
  });

  it('keeps one option per label id through a rename, and writes nothing for it', async () => {
    setBindingMock('SearchPRCandidates', async () => ({ people: [], labels: [{ id: 'LA_1', name: 'defect' }], more: false }));
    const set = setBindingMock('SetPRLabels', async () => ({ seq: 0 }));
    view.picker.open('labels', { labels: [{ id: 'LA_1', name: 'bug' }] });
    await settle();
    expect(view.picker.options.map((option) => [option.key, option.checked])).toEqual([['l:LA_1', true]]);
    await view.picker.close();
    expect(set).not.toHaveBeenCalled();
  });

  it('offers teams only when asked to', async () => {
    const team = { login: 'owner/core', team: true };
    setBindingMock('SearchPRCandidates', async () => ({ people: [JDOE, team], labels: [], more: false }));
    view.picker.open('reviewers', {});
    await settle();
    expect(view.picker.options.map((option) => option.key)).toEqual(['u:jdoe']);
    await view.picker.close();
    view.picker.open('reviewers', { teams: true });
    await settle();
    expect(view.picker.options.map((option) => option.key)).toEqual(['u:jdoe', 't:owner/core']);
  });

  it('shows a failed change on its row until the row is picked again', async () => {
    setBindingMock('SearchPRCandidates', async () => ({ people: [], labels: [{ name: 'feature', id: 'LA_9' }], more: false }));
    setBindingMock('SetPRLabels', async () => { throw new TransportError('forge_transient', 'GitHub applied part of the change to the labels and then timed out. The pane shows what GitHub did.'); });
    view.picker.open('labels', { labels: [] });
    await settle();
    view.picker.toggle('l:LA_9');
    await view.picker.close();
    expect(view.rowError('labels')?.message).toContain('applied part of the change');
    view.picker.open('labels', { labels: [] });
    expect(view.rowError('labels')).toBeNull();
  });
});

describe('the merge commit message', () => {
  it('sends only an edited message, for a method that makes a commit', async () => {
    const read = setBindingMock('GetPRMergeMessage', async (_pr: unknown, method: string) => ({ title: `Default ${method}`, body: 'Body' }));
    const message = view.mergeMessage;
    expect(message.messageFor('merge')).toBeUndefined();
    message.openFor('merge');
    await settle();
    expect(message.title).toBe('Default merge');
    expect(message.messageFor('merge')).toBeUndefined();
    // Untouched, a method change reads that method's default.
    message.pick('squash');
    await settle();
    expect(read.mock.calls.map((callArgs) => callArgs[1])).toEqual(['merge', 'squash']);
    expect(message.title).toBe('Default squash');
    message.setTitle('Mine\nsplit');
    expect(message.messageFor('squash')).toEqual({ title: 'Mine split', body: 'Body' });
    expect(message.messageFor('rebase')).toBeUndefined();
    // Edited, a method change keeps the text and reads nothing; the text
    // goes with the method the editor shows, never another.
    message.pick('merge');
    await settle();
    expect(read).toHaveBeenCalledTimes(2);
    expect(message.title).toBe('Mine split');
    expect(message.messageFor('merge')).toEqual({ title: 'Mine split', body: 'Body' });
    expect(message.messageFor('squash')).toBeUndefined();
    message.close();
    expect(message.messageFor('merge')).toBeUndefined();
  });

  it('shows a failed default read and leaves the merge as it was', async () => {
    setBindingMock('GetPRMergeMessage', async () => { throw new TransportError('forge_transient', 'GitHub did not answer. Try again in a moment.'); });
    view.mergeMessage.openFor('merge');
    await settle();
    expect(view.mergeMessage.error?.message).toBe('GitHub did not answer. Try again in a moment.');
    expect(view.mergeMessage.messageFor('merge')).toBeUndefined();
  });
});

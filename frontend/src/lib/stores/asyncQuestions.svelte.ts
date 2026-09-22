import { ListAsyncQuestions, SubmitAsyncAnswers, SetAsyncQuestionDismissed, AsyncQuestionAnswer, type AsyncQuestion } from './bindings';
import { createEntityStore } from './entityStore.svelte';
import { threadBackend, onThreadOwnershipChanged } from '../transport/entityIndex';
import { withBackendTarget } from '../transport/backends';
import { holdBackendRecovery } from './transportRecovery';
import { TransportError } from '../transport/wsClient';
import { isAlreadyHandled } from '../transport/alreadyHandled';
import type { BackendKey } from '../transport/backendKey';
import { randomId } from '../utils/randomId';

export type { AsyncQuestion } from './bindings';
export const questionKey = (q: Pick<AsyncQuestion, 'itemId' | 'index'>): string => JSON.stringify([q.itemId, q.index]);
const keyFor = (threadId: string, itemId: string) => JSON.stringify([threadId, itemId]);
const decodeKey = (key: string): [string, string] => JSON.parse(key);

const questions = createEntityStore<AsyncQuestion[], void>({
  name: 'async questions', rawValue: true,
  backendForKey: key => threadBackend(decodeKey(key)[0]),
  source: async ({ key, apply }) => {
    const [threadId, itemId] = decodeKey(key);
    const read = ListAsyncQuestions(threadId, itemId).then(rows => { apply(rows ?? []); });
    const backend = threadBackend(threadId);
    if (backend !== undefined) holdBackendRecovery(backend, read);
    await read;
    return () => {};
  },
});

export function attachAsyncQuestions(threadId: string, itemId = '') {
  return questions.attach(keyFor(threadId, itemId), undefined);
}

export function refreshAsyncQuestions(threadId?: string, backend?: BackendKey): void {
  for (const key of questions.keys()) {
    if ((!threadId || decodeKey(key)[0] === threadId) && (backend === undefined || threadBackend(decodeKey(key)[0]) === backend)) questions.invalidate(key);
  }
}

onThreadOwnershipChanged(threadId => refreshAsyncQuestions(threadId));

export async function dismissAsyncQuestion(threadId: string, question: Pick<AsyncQuestion, 'itemId' | 'index'>, dismissed: boolean): Promise<void> {
  await SetAsyncQuestionDismissed(threadId, question.itemId, question.index, dismissed);
  refreshAsyncQuestions(threadId);
}

interface Submission { sendId: string; answers: { itemId: string; index: number; answer: string }[] }
interface SavedDraft { answers: Record<string, string>; active: string; pending: Submission | null }

export type AsyncQuestionDraft = ReturnType<typeof createDraft>;
const drafts = new Map<string, { draft: AsyncQuestionDraft; refs: number }>();

// Mounted consumers and in-flight submissions share one draft; late replies from
// a previous mount cannot overwrite edits made after returning to the thread.
export function attachAsyncQuestionDraft(threadId: string) {
  const backend = threadBackend(threadId);
  if (backend === undefined) throw new Error('The question’s computer is not known yet');
  const key = JSON.stringify([backend, threadId]);
  let entry = drafts.get(key);
  const collect = () => {
    const current = drafts.get(key);
    if (current && current.refs === 0 && !current.draft.sending) drafts.delete(key);
  };
  if (!entry) {
    entry = { draft: createDraft(threadId, backend, `agent-overflow:question-draft:${key}`, collect), refs: 0 };
    drafts.set(key, entry);
  }
  entry.refs++;
  let released = false;
  return { draft: entry.draft, release() { if (!released) { released = true; entry.refs--; collect(); } } };
}

function createDraft(threadId: string, backend: BackendKey, storageKey: string, collect: () => void) {
  let saved: SavedDraft = { answers: {}, active: '', pending: null };
  const raw = localStorage.getItem(storageKey);
  if (raw !== null) {
    const parsed = JSON.parse(raw) as SavedDraft;
    if (!parsed || !parsed.answers || typeof parsed.answers !== 'object' || Array.isArray(parsed.answers) || typeof parsed.active !== 'string'
      || Object.values(parsed.answers).some(value => typeof value !== 'string')) throw new Error('Invalid saved question draft');
    if (parsed.pending != null && (typeof parsed.pending.sendId !== 'string' || !parsed.pending.sendId
      || !Array.isArray(parsed.pending.answers) || !parsed.pending.answers.length
      || parsed.pending.answers.some(a => !a || typeof a.itemId !== 'string' || !a.itemId
        || !Number.isInteger(a.index) || a.index < 0 || typeof a.answer !== 'string' || !a.answer.trim()))) {
      throw new Error('Invalid saved question submission');
    }
    saved = { ...parsed, pending: parsed.pending ?? null };
  }
  let answers = $state<Record<string, string>>(saved.answers);
  let active = $state(saved.active);
  let pending = $state<Submission | null>(saved.pending);
  let sending = $state(false);
  function save(): void { localStorage.setItem(storageKey, JSON.stringify({ answers, active, pending })); }
  return {
    get answers() { return answers; },
    get active() { return active; },
    get sending() { return sending; },
    get retrying() { return pending !== null; },
    isPending(q: Pick<AsyncQuestion, 'itemId' | 'index'>) {
      return pending?.answers.some(a => questionKey(a) === questionKey(q)) ?? false;
    },
    focus(key: string) { active = key; save(); },
    answer(key: string, value: string) { active = key; answers = { ...answers, [key]: value }; save(); },
    async submit(rows: readonly AsyncQuestion[]) {
      if (sending) return;
      if (threadBackend(threadId) !== backend) throw new Error('This conversation moved to another computer. Reopen its questions before answering.');
      const snapshot = pending ?? {
        sendId: `async-answer:${randomId()}`,
        answers: rows.filter(q => q.state === 'unanswered' && answers[questionKey(q)]?.trim())
          .map(q => ({ itemId: q.itemId, index: q.index, answer: answers[questionKey(q)] })),
      };
      if (!snapshot.answers.length) return;
      pending = snapshot;
      save();
      sending = true;
      try {
        await withBackendTarget(backend, () => SubmitAsyncAnswers(threadId, snapshot.sendId, snapshot.answers.map(a => new AsyncQuestionAnswer(a))));
        const next = { ...answers };
        for (const answer of snapshot.answers) {
          const key = questionKey(answer);
          if (next[key] === answer.answer) delete next[key];
        }
        answers = next; pending = null; save();
        refreshAsyncQuestions(threadId);
      } catch (error) {
        if (isAlreadyHandled(error)) {
          pending = null; save(); refreshAsyncQuestions(threadId);
          throw new Error('Some questions were already answered or dismissed on another screen. Your other answers are saved; review and send them again.');
        }
        if (error instanceof TransportError && error.code !== 'timeout') {
          pending = null; save(); refreshAsyncQuestions(threadId);
        }
        throw error;
      } finally { sending = false; collect(); }
    },
  };
}

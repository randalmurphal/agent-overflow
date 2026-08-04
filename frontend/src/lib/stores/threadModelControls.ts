// The one place a thread's model, reasoning effort, and fast mode change.
//
// Extracted from the composer toolbar's two pickers so the composer's
// `/model`, `/effort`, and `/fast` commands drive the exact same code —
// including the parts that are easy to forget: a draft placeholder updates its
// defaults instead of a thread row, and re-selecting the model a fallback
// displaced means "try my preferred model again", which is a session
// reconnect rather than a no-op write.
//
// Each function reports its own failure as a string instead of raising a
// toast: the pickers want a toast, the composer wants composer-local state
// next to the text the user typed, and only the caller knows which.

import {
  ReconnectSession,
  UpdateThreadContextWindow,
  UpdateThreadFastMode,
  UpdateThreadModelSelection,
  UpdateThreadReasoningEffort,
} from './bindings';
import { syncThread } from './panes.svelte';
import { updatePlaceholderDefaults } from './newThreadDefaults';
import type { ThreadPane } from './thread.svelte';
import type { Thread } from '../types/models';
import type { ProviderID } from '../types/providers';
import { errString } from '../utils/errors';

/** Outcome of an apply. `error` is user-facing text, already formatted. */
export interface ThreadControlResult {
  ok: boolean;
  error?: string;
}

const OK: ThreadControlResult = { ok: true };

function failure(action: string, err: unknown): ThreadControlResult {
  console.error(`${action} failed:`, err);
  return { ok: false, error: `${action}: ${errString(err)}` };
}

/**
 * Point the thread at `provider`/`slug`.
 *
 * Selecting the model the thread already requested while a classifier
 * fallback is serving it restarts the session rather than issuing a write the
 * backend would treat as a no-op — the durable selection is already correct
 * and what the user wants is another attempt at it.
 */
export async function applyThreadModelSelection(
  pane: ThreadPane,
  provider: ProviderID,
  slug: string,
): Promise<ThreadControlResult> {
  if (!pane.thread) return { ok: false, error: 'Open a thread first' };
  const currentProvider = pane.thread.provider;
  const currentModel = pane.thread.model;
  if (provider === currentProvider && slug === currentModel && slug === pane.activeModel) {
    return OK;
  }
  try {
    if (pane.hasDraftPlaceholder) {
      await updatePlaceholderDefaults(pane, { provider, model: slug });
      return OK;
    }
    const threadId = pane.threadId;
    if (!threadId) return { ok: false, error: 'Start the thread first' };
    if (provider === currentProvider && slug === currentModel) {
      await ReconnectSession(threadId);
      pane.setEffectiveModel('');
      return OK;
    }
    const updated = (await UpdateThreadModelSelection(threadId, provider, slug)) as Thread;
    syncThread(updated);
    return OK;
  } catch (err) {
    return failure('Failed to switch model', err);
  }
}

export async function applyThreadReasoningEffort(
  pane: ThreadPane,
  effort: string,
): Promise<ThreadControlResult> {
  if (!pane.thread) return { ok: false, error: 'Open a thread first' };
  if (effort === (pane.thread.reasoningEffort ?? '')) return OK;
  try {
    if (pane.hasDraftPlaceholder) {
      await updatePlaceholderDefaults(pane, { reasoningEffort: effort });
      return OK;
    }
    const threadId = pane.threadId;
    if (!threadId) return { ok: false, error: 'Start the thread first' };
    const updated = (await UpdateThreadReasoningEffort(threadId, effort)) as Thread;
    syncThread(updated);
    return OK;
  } catch (err) {
    return failure('Failed to set effort', err);
  }
}

export async function applyThreadFastMode(
  pane: ThreadPane,
  on: boolean,
): Promise<ThreadControlResult> {
  if (!pane.thread) return { ok: false, error: 'Open a thread first' };
  if (on === (pane.thread.fastMode === true)) return OK;
  try {
    if (pane.hasDraftPlaceholder) {
      await updatePlaceholderDefaults(pane, { fastMode: on });
      return OK;
    }
    const threadId = pane.threadId;
    if (!threadId) return { ok: false, error: 'Start the thread first' };
    const updated = (await UpdateThreadFastMode(threadId, on)) as Thread;
    syncThread(updated);
    return OK;
  } catch (err) {
    return failure('Failed to set fast mode', err);
  }
}

export async function applyThreadContextWindow(
  pane: ThreadPane,
  tokens: number,
): Promise<ThreadControlResult> {
  if (!pane.thread) return { ok: false, error: 'Open a thread first' };
  if (tokens === (pane.thread.contextWindow ?? 0)) return OK;
  try {
    if (pane.hasDraftPlaceholder) {
      await updatePlaceholderDefaults(pane, { contextWindow: tokens });
      return OK;
    }
    const threadId = pane.threadId;
    if (!threadId) return { ok: false, error: 'Start the thread first' };
    const updated = (await UpdateThreadContextWindow(threadId, tokens)) as Thread;
    syncThread(updated);
    return OK;
  } catch (err) {
    return failure('Failed to set context window', err);
  }
}

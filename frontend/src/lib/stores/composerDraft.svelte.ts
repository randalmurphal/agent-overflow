import type { Attachment } from '../types/attachment';
import type { Draft, TerminalChip } from '../types/draft';
import {
  ClearDraft,
  GetDraft,
  ListAttachments,
  SaveDraft,
} from './bindings';
import { errString } from '../utils/errors';
import { ensureImagePlaceholders } from '../utils/imagePlaceholders';

const DEFAULT_DEBOUNCE_MS = 500;

interface DraftStoreOptions {
  debounceMs?: number;
  getNow?: () => number;
}

/**
 * Per-thread composer draft state. One instance manages its own timers and
 * requests; the caller is expected to call `setThread` whenever the active
 * thread changes so we can hydrate / flush correctly.
 */
export function createComposerDraftStore(options: DraftStoreOptions = {}) {
  const debounceMs = options.debounceMs ?? DEFAULT_DEBOUNCE_MS;
  const now = options.getNow ?? Date.now;

  let threadId: string | null = $state(null);
  let content: string = $state('');
  let attachments: Attachment[] = $state([]);
  let terminalChips: TerminalChip[] = $state([]);
  let hydrating: boolean = $state(false);
  let error: string | null = $state(null);

  let debounceTimer: ReturnType<typeof setTimeout> | null = null;
  let pendingSaveGeneration = 0;

  function clearDebounce() {
    if (debounceTimer) {
      clearTimeout(debounceTimer);
      debounceTimer = null;
    }
  }

  async function loadAttachments(id: string): Promise<Attachment[]> {
    const records = (await ListAttachments(id)) as Attachment[] | null;
    return records ?? [];
  }

  async function hydrate(id: string): Promise<void> {
    hydrating = true;
    error = null;
    try {
      const [draft, records] = await Promise.all([
        GetDraft(id) as Promise<Draft>,
        loadAttachments(id),
      ]);
      if (threadId !== id) return; // thread switched while loading

      const attachmentIds = new Set(draft.attachmentIds ?? []);
      attachments = records.filter((rec) => attachmentIds.has(rec.id));
      terminalChips = draft.terminalChips ?? [];
      content = ensureImagePlaceholders(draft.content ?? '', attachments);
    } catch (err) {
      error = `Failed to load draft: ${errString(err)}`;
    } finally {
      if (threadId === id) {
        hydrating = false;
      }
    }
  }

  function queueSave(): void {
    if (!threadId) return;
    clearDebounce();
    const id = threadId;
    const generation = ++pendingSaveGeneration;
    debounceTimer = setTimeout(async () => {
      debounceTimer = null;
      if (threadId !== id || generation !== pendingSaveGeneration) return;
      try {
        await SaveDraft(
          id,
          content,
          attachments.map((a) => a.id),
          terminalChips,
        );
      } catch (err) {
        error = `Failed to save draft: ${errString(err)}`;
      }
    }, debounceMs);
  }

  async function flush(): Promise<void> {
    if (!threadId) return;
    clearDebounce();
    const id = threadId;
    try {
      await SaveDraft(
        id,
        content,
        attachments.map((a) => a.id),
        terminalChips,
      );
    } catch (err) {
      error = `Failed to save draft: ${errString(err)}`;
    }
  }

  async function setThread(id: string | null): Promise<void> {
    if (threadId === id) return;
    clearDebounce();
    pendingSaveGeneration++;
    threadId = id;
    content = '';
    attachments = [];
    terminalChips = [];
    error = null;
    if (id) {
      await hydrate(id);
    }
  }

  return {
    // ---- reads ----
    get threadId() { return threadId; },
    get content() { return content; },
    get attachments() { return attachments; },
    get terminalChips() { return terminalChips; },
    get hydrating() { return hydrating; },
    get error() { return error; },
    get hasDraft() {
      return content.trim().length > 0 || attachments.length > 0 || terminalChips.length > 0;
    },

    // ---- thread lifecycle ----
    setThread,
    flush,

    // ---- content mutations ----
    setContent(next: string): void {
      content = next;
      queueSave();
    },

    setContentAndAttachments(nextContent: string, nextAttachments: Attachment[]): void {
      content = nextContent;
      attachments = [...nextAttachments];
      queueSave();
    },

    removeAttachment(id: string): void {
      const next = attachments.filter((a) => a.id !== id);
      if (next.length === attachments.length) return;
      attachments = next;
      queueSave();
    },

    addTerminalChip(chip: TerminalChip): void {
      if (terminalChips.some((c) => c.id === chip.id)) return;
      terminalChips = [...terminalChips, chip];
      queueSave();
    },

    removeTerminalChip(id: string): void {
      const next = terminalChips.filter((c) => c.id !== id);
      if (next.length === terminalChips.length) return;
      terminalChips = next;
      queueSave();
    },

    setError(message: string | null): void {
      error = message;
    },

    /**
     * Called after a successful Send. Clears local state and the backend
     * row so the thread re-loads empty next time.
     */
    async clearAfterSend(): Promise<void> {
      const id = threadId;
      clearDebounce();
      pendingSaveGeneration++;
      content = '';
      attachments = [];
      terminalChips = [];
      if (!id) return;
      try {
        await ClearDraft(id);
      } catch (err) {
        error = `Failed to clear draft: ${errString(err)}`;
      }
    },

    /**
     * Restore a draft snapshot to a specific thread. Used when a send
     * rejects AFTER the user has switched panes: we don't want to silently
     * dump thread A's failed message into thread B's composer. If the draft
     * store is still on the given thread we also restore the local UI state
     * so the composer re-populates immediately; otherwise the backend row
     * is repopulated so the user sees the draft next time they return.
     */
    async restoreDraftFor(
      id: string,
      snapshot: { content: string; attachments: Attachment[]; terminalChips: TerminalChip[] },
    ): Promise<void> {
      // Persist the snapshot back to the backend regardless of active thread
      // so the draft lives across thread switches.
      try {
        await SaveDraft(
          id,
          snapshot.content,
          snapshot.attachments.map((a) => a.id),
          snapshot.terminalChips,
        );
      } catch (err) {
        error = `Failed to restore draft: ${errString(err)}`;
        return;
      }
      // If the store is still pointed at the same thread, mirror the
      // snapshot into the local state so the composer shows it right away.
      if (threadId === id) {
        clearDebounce();
        pendingSaveGeneration++;
        content = snapshot.content;
        attachments = [...snapshot.attachments];
        terminalChips = [...snapshot.terminalChips];
      }
    },

    /**
     * Build the outgoing text payload for Send. Terminal chip contents
     * are inlined as fenced blocks; image attachments travel separately as
     * structured attachment ids so providers receive real image inputs.
     */
    composeOutgoingMessage(): string {
      const messageContent = ensureImagePlaceholders(content, attachments);
      const chipsBlock = terminalChips
        .map((chip) => `\n\n\`\`\`terminal ${chip.label}\n${chip.content}\n\`\`\``)
        .join('');
      return messageContent + chipsBlock;
    },
  };
}

export type ComposerDraftStore = ReturnType<typeof createComposerDraftStore>;

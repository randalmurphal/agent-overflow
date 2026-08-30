import type { Item } from '../types/models';
import {
  createProvenAppend,
  matchesProvenAppend,
  type ProvenAppend,
} from '../markdown';
import {
  StreamingAssistantRevealRouter,
  type StreamingAssistantCommitMode,
  type StreamingAssistantRenderContext,
  type StreamingAssistantRevealSink,
} from './streamingAssistantReveal';

interface AssistantAppendRecord {
  /** Canonical reveal rope. Never hand this string to the Markdown parser. */
  currentSource: string;
  /** Last independent string handed to the parser. */
  parserSource: string;
  /** Raw reveal units since parserSource, retained once and joined on demand. */
  pendingParserDeltas: string[];
  publishedAppend: ProvenAppend | undefined;
  publishedGeneration: number;
}

export interface ThreadAssistantRevealOptions {
  getItemIndex(itemId: string): number | undefined;
  getItems(): Item[];
  setItemAt(index: number, item: Item): void;
  hasSmoother(itemId: string): boolean;
}

export interface ThreadAssistantReveal {
  readonly registrationGeneration: number;
  register(itemId: string, sink: StreamingAssistantRevealSink): () => void;
  parserSource(
    itemId: string,
    canonicalSource: string,
    renderContext: StreamingAssistantRenderContext,
  ): string;
  sourceAppend(itemId: string, source: string): ProvenAppend | undefined;
  publish(
    itemId: string,
    previousCodeUnit: number,
    source: string,
    delta: string,
    commitCanonical: (
      nextSource: string,
      mode: StreamingAssistantCommitMode,
      append: ProvenAppend,
    ) => void,
  ): boolean;
  commitAuthoritativeAppend(
    itemId: string,
    previousSource: string,
    delta: string,
    commit: (nextSource: string) => void,
  ): void;
  reconcileItemWrite(previous: Item, next: Item): void;
  clearPresentation(itemId: string): void;
  discardItem(itemId: string): void;
  disposeAll(): void;
  pruneRecords(retainedItemIds: ReadonlySet<string>): void;
}

/**
 * Pane-local bridge between canonical assistant rows, parser checkpoints, and
 * mounted direct-DOM sinks. Append lineage lives here with the router so each
 * transition updates both or fails before either can become stale.
 */
export function createThreadAssistantReveal(
  options: ThreadAssistantRevealOptions,
): ThreadAssistantReveal {
  const router = new StreamingAssistantRevealRouter();
  // A same-thread reload keeps pane, row, and item identities stable while a
  // pane-wide dispose clears the sink registry. This reactive edge makes the
  // mounted rows register again even when disposal reports a reset error.
  let registrationGeneration = $state(0);
  // Canonical reveal strings remain an uninspected cons rope. Markdown lexing
  // flattens its input; handing it that rope mutates every checkpoint into a
  // full flat prefix, and later appends retain all of those prefixes through
  // the final rope. Records therefore keep raw reveal units and materialize a
  // separate parser string only when Svelte needs an authoritative render.
  // The parser still receives opaque append lineage over that independent
  // string, so parseBlocks keeps its suffix-only path.
  const appendByItem = new Map<string, AssistantAppendRecord>();

  function publishPending(itemId: string, record: AssistantAppendRecord): void {
    if (record.pendingParserDeltas.length === 0) return;
    const delta = record.pendingParserDeltas.length === 1
      ? record.pendingParserDeltas[0]
      : record.pendingParserDeltas.join('');
    // Keep the parser source separate from the canonical per-reveal rope, but
    // do not copy its complete prefix. ChatMarkdown's streaming boundary
    // splitter consumes this proof and hands marked only its bounded committed
    // and volatile pieces. The full parser rope is parsed once after settle,
    // when it can no longer retain a chain of later checkpoints.
    record.publishedAppend = createProvenAppend(
      record.parserSource,
      delta,
    );
    record.parserSource = record.publishedAppend.next;
    record.pendingParserDeltas = [];
    const publishedGeneration = ++record.publishedGeneration;
    // The first microtask yields to Svelte's row flush. The second releases
    // the suffix after every mounted projection has consumed it.
    queueMicrotask(() => queueMicrotask(() => {
      if (
        appendByItem.get(itemId) !== record ||
        record.publishedGeneration !== publishedGeneration
      ) return;
      record.publishedAppend = undefined;
      if (!options.hasSmoother(itemId) && record.pendingParserDeltas.length === 0) {
        appendByItem.delete(itemId);
      }
    }));
  }

  function stageAppend(
    itemId: string,
    append: ProvenAppend,
    mode: StreamingAssistantCommitMode,
  ): void {
    const { previous: previousSource, next: nextSource } = append;
    let record = appendByItem.get(itemId);
    if (!record || record.currentSource !== previousSource) {
      record = {
        currentSource: previousSource,
        parserSource: previousSource,
        pendingParserDeltas: [],
        publishedAppend: undefined,
        publishedGeneration: 0,
      };
      appendByItem.set(itemId, record);
    }
    if (record.currentSource !== nextSource) {
      if (!matchesProvenAppend(append, previousSource, nextSource)) {
        appendByItem.delete(itemId);
        throw new Error(`assistant append lineage mismatch for ${itemId}`);
      }
      record.pendingParserDeltas.push(append.delta);
      record.currentSource = nextSource;
    }
    if (mode === 'authoritative') publishPending(itemId, record);
  }

  function register(
    itemId: string,
    sink: StreamingAssistantRevealSink,
  ): () => void {
    return router.register(itemId, sink, () => {
      const index = options.getItemIndex(itemId);
      if (index === undefined) {
        throw new Error(`streaming assistant reveal cannot restore missing item ${itemId}`);
      }
      const current = options.getItems()[index];
      if (!current || current.id !== itemId) {
        throw new Error(`streaming assistant reveal cannot restore missing item ${itemId}`);
      }
      // The router dropped its checkpoint. An equivalent row replacement
      // makes every mounted projection read the canonical source again.
      options.setItemAt(index, { ...current });
    });
  }

  function parserSource(
    itemId: string,
    canonicalSource: string,
    renderContext: StreamingAssistantRenderContext,
  ): string {
    const source = router.parserSourceFor(itemId, canonicalSource, renderContext);
    const record = appendByItem.get(itemId);
    if (record?.currentSource === canonicalSource) {
      // A shorter router source is the standing parser checkpoint while the
      // canonical row carries a direct DOM suffix. Equal lengths mean the
      // router requested the current authoritative source. Length is enough:
      // every transition is append-only inside this record, and comparing the
      // two string values would flatten the canonical rope we are protecting.
      if (source.length === canonicalSource.length) {
        publishPending(itemId, record);
      }
      return record.parserSource;
    }
    return source;
  }

  function sourceAppend(itemId: string, source: string): ProvenAppend | undefined {
    const record = appendByItem.get(itemId);
    const append = record?.publishedAppend;
    return append?.next === source ? append : undefined;
  }

  function publish(
    itemId: string,
    previousCodeUnit: number,
    source: string,
    delta: string,
    commitCanonical: (
      nextSource: string,
      mode: StreamingAssistantCommitMode,
      append: ProvenAppend,
    ) => void,
  ): boolean {
    return router.publish(
      itemId,
      previousCodeUnit,
      source,
      delta,
      (nextSource, mode, append) => {
        stageAppend(itemId, append, mode);
        try {
          commitCanonical(nextSource, mode, append);
        } catch (error) {
          appendByItem.delete(itemId);
          throw error;
        }
      },
    );
  }

  function commitAuthoritativeAppend(
    itemId: string,
    previousSource: string,
    delta: string,
    commit: (nextSource: string) => void,
  ): void {
    const append = createProvenAppend(previousSource, delta);
    stageAppend(itemId, append, 'authoritative');
    try {
      commit(append.next);
    } catch (error) {
      appendByItem.delete(itemId);
      throw error;
    }
  }

  function reconcileItemWrite(previous: Item, next: Item): void {
    router.reconcileItemWrite(previous, next);
    const record = appendByItem.get(next.id);
    if (
      record &&
      (next.kind !== 'assistant_text' || next.summary !== record.currentSource)
    ) {
      appendByItem.delete(next.id);
    } else if (record) {
      publishPending(next.id, record);
    }
  }

  function discardItem(itemId: string): void {
    try {
      router.clearItem(itemId);
    } finally {
      appendByItem.delete(itemId);
    }
  }

  function clearPresentation(itemId: string): void {
    router.retireItem(itemId, () => {
      const index = options.getItemIndex(itemId);
      if (index === undefined) {
        throw new Error(`streaming assistant reveal cannot retire missing item ${itemId}`);
      }
      const current = options.getItems()[index];
      if (!current || current.id !== itemId) {
        throw new Error(`streaming assistant reveal cannot retire missing item ${itemId}`);
      }
      // A fresh row identity is the reactive handoff from sink-owned Text
      // nodes back to the markdown parser's complete canonical source.
      options.setItemAt(index, { ...current });
    });
  }

  function disposeAll(): void {
    appendByItem.clear();
    try {
      router.dispose();
    } finally {
      registrationGeneration++;
    }
  }

  function pruneRecords(retainedItemIds: ReadonlySet<string>): void {
    for (const itemId of appendByItem.keys()) {
      if (retainedItemIds.has(itemId) || options.hasSmoother(itemId)) continue;
      appendByItem.delete(itemId);
    }
  }

  return {
    get registrationGeneration() {
      return registrationGeneration;
    },
    register,
    parserSource,
    sourceAppend,
    publish,
    commitAuthoritativeAppend,
    reconcileItemWrite,
    clearPresentation,
    discardItem,
    disposeAll,
    pruneRecords,
  };
}

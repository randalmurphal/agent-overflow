import { reportFrontendDiagnostic } from '../utils/frontendErrorCapture';
import { createProvenAppend, type ProvenAppend } from '../markdown';
import {
  isAssistantLiteralTailCodeUnit,
  isSafeAssistantLiteralDelta,
  isSafeAssistantLiteralPredecessor,
} from './streamingAssistantLiteralSafety';

export interface StreamingAssistantParserCheckpoint {
  /** A bounded suffix from the reveal unit that established the parser tree. */
  readonly tailSource: string;
  readonly tailStart: number;
  readonly tailEnd: number;
  /** ASCII spaces present in source after `tailEnd`. */
  readonly trailingAsciiSpaces: number;
}

export interface StreamingAssistantRevealSink {
  /**
   * Verify that this mounted representation still renders `source` at a
   * literal text leaf that can accept `delta` without changing markdown
   * structure. This is a read-only preflight. The router calls every sink
   * before it mutates the canonical item or any DOM.
   */
  canAppendLiteral(
    source: string,
    checkpoint: StreamingAssistantParserCheckpoint,
    nextSource: string,
    delta: string,
  ): boolean;
  /** Append a delta that already passed every sink's preflight. */
  appendLiteral(nextSource: string, delta: string): void;
  /**
   * Rebuild sink-owned DOM after another representation kept the parser
   * checkpoint alive while this one was unmounted.
   */
  restoreLiteral(
    parserSource: string,
    checkpoint: StreamingAssistantParserCheckpoint,
    source: string,
    directDeltas: readonly string[],
  ): boolean;
  /** Reconcile DOM owned by the direct path before an authoritative render. */
  reset(): void;
}

export interface StreamingAssistantRenderContext {
  /** Whether incomplete-markdown parsing and the committed/tail split are active. */
  streaming: boolean;
  /** Whether Settings permits the volatile tail to remain mounted. */
  volatileTailVisible: boolean;
  /** Whether path links are inert for this page — true when the session
   *  cannot act on the host desktop, which is where an editor open lands. */
  pathLinksInert: boolean;
  /** Base path used by the markdown link extension. */
  workspacePath: string;
}

export type StreamingAssistantCommitMode = 'authoritative' | 'direct';

interface DirectPresentation {
  /** Source owned by Svelte and the markdown parser. */
  parserSource: string;
  /** Canonical source represented by parserSource plus sink-owned DOM. */
  expectedSource: string;
  /** Bounded parser-tail proof. It never reads the growing canonical rope. */
  parserCheckpoint: StreamingAssistantParserCheckpoint;
  /** Tail state after `directDeltas`, updated in place without per-tick objects. */
  expectedCheckpoint: MutableParserCheckpoint;
  /** Exact append units since parserSource, used to restore a remounted sink. */
  directDeltas: string[];
  /** Context of the authoritative render that established parserSource. */
  renderContext?: StreamingAssistantRenderContext;
}

type MutableParserCheckpoint = {
  -readonly [Key in keyof StreamingAssistantParserCheckpoint]:
    StreamingAssistantParserCheckpoint[Key];
};

const CHECKPOINT_TAIL_CODE_UNITS = 128;

function trailingAsciiSpaces(value: string): number {
  let index = value.length;
  while (index > 0 && value.charCodeAt(index - 1) === 32) index--;
  return value.length - index;
}

function checkpointFromDelta(delta: string): MutableParserCheckpoint | undefined {
  const spaces = trailingAsciiSpaces(delta);
  const tailEnd = delta.length - spaces;
  if (tailEnd === 0) return undefined;
  let literalStart = tailEnd;
  while (literalStart > 0) {
    const code = delta.charCodeAt(literalStart - 1);
    if (!isAssistantLiteralTailCodeUnit(code)) break;
    literalStart--;
  }
  if (literalStart === tailEnd) return undefined;
  return {
    tailSource: delta,
    tailStart: Math.max(literalStart, tailEnd - CHECKPOINT_TAIL_CODE_UNITS),
    tailEnd,
    trailingAsciiSpaces: spaces,
  };
}

function advanceCheckpoint(
  checkpoint: StreamingAssistantParserCheckpoint,
  delta: string,
): MutableParserCheckpoint {
  const spaces = trailingAsciiSpaces(delta);
  const tailEnd = delta.length - spaces;
  if (tailEnd === 0) {
    return {
      ...checkpoint,
      trailingAsciiSpaces: checkpoint.trailingAsciiSpaces + spaces,
    };
  }
  return {
    tailSource: delta,
    tailStart: Math.max(0, tailEnd - CHECKPOINT_TAIL_CODE_UNITS),
    tailEnd,
    trailingAsciiSpaces: spaces,
  };
}

function advanceCheckpointInPlace(
  checkpoint: MutableParserCheckpoint,
  delta: string,
): void {
  const spaces = trailingAsciiSpaces(delta);
  const tailEnd = delta.length - spaces;
  if (tailEnd === 0) {
    checkpoint.trailingAsciiSpaces += spaces;
    return;
  }
  checkpoint.tailSource = delta;
  checkpoint.tailStart = Math.max(0, tailEnd - CHECKPOINT_TAIL_CODE_UNITS);
  checkpoint.tailEnd = tailEnd;
  checkpoint.trailingAsciiSpaces = spaces;
}

function createDirectPresentation(
  source: string,
  checkpoint: StreamingAssistantParserCheckpoint,
): DirectPresentation {
  return {
    parserSource: source,
    expectedSource: source,
    parserCheckpoint: checkpoint,
    expectedCheckpoint: { ...checkpoint },
    directDeltas: [],
  };
}

function sameRenderContext(
  left: StreamingAssistantRenderContext,
  right: StreamingAssistantRenderContext,
): boolean {
  return left.streaming === right.streaming &&
    left.volatileTailVisible === right.volatileTailVisible &&
    left.pathLinksInert === right.pathLinksInert &&
    left.workspacePath === right.workspacePath;
}

/**
 * One reveal router belongs to one thread-pane store. Item ids are globally
 * stable, so a process-global registry would route duplicate projections
 * through each other and append every reveal twice.
 *
 * The canonical raw item remains current on every reveal. The router holds a
 * non-reactive parser checkpoint while every mounted assistant-body sink
 * paints the literal suffix after it. Safe suffixes intentionally leave the
 * row signal quiet. An authoritative reveal wakes the row and gives
 * Streamdown the complete source when markdown structure can change.
 *
 * Every transition is owned here. An unsafe delta clears sink DOM before the
 * authoritative row write. A remount restores the suffix from the canonical
 * source. A write that disagrees with the expected source clears the
 * checkpoint, so callers cannot leave a stale masked parser behind.
 */
export class StreamingAssistantRevealRouter {
  private readonly sinksByItem = new Map<string, Set<StreamingAssistantRevealSink>>();
  private readonly presentationByItem = new Map<string, DirectPresentation>();

  register(
    itemId: string,
    sink: StreamingAssistantRevealSink,
    requestAuthoritativeRender: () => void,
  ): () => void {
    let sinks = this.sinksByItem.get(itemId);
    if (!sinks) {
      sinks = new Set();
      this.sinksByItem.set(itemId, sinks);
    }
    if (sinks.has(sink)) {
      throw new Error(`streaming assistant reveal sink already registered for ${itemId}`);
    }
    sinks.add(sink);

    const presentation = this.presentationByItem.get(itemId);
    if (presentation) {
      let restored = false;
      try {
        restored = sink.restoreLiteral(
          presentation.parserSource,
          presentation.parserCheckpoint,
          presentation.expectedSource,
          presentation.directDeltas,
        );
      } catch (error) {
        const recoveryErrors: unknown[] = [error];
        try {
          this.resetItem(itemId);
        } catch (resetError) {
          recoveryErrors.push(resetError);
        }
        try {
          requestAuthoritativeRender();
        } catch (renderError) {
          recoveryErrors.push(renderError);
        }
        this.removeSink(itemId, sink);
        if (recoveryErrors.length === 1) throw error;
        throw new AggregateError(
          recoveryErrors,
          `streaming assistant reveal registration recovery failed for ${itemId}`,
        );
      }
      if (!restored) {
        const recoveryErrors: unknown[] = [];
        try {
          this.resetItem(itemId);
        } catch (error) {
          recoveryErrors.push(error);
        }
        try {
          requestAuthoritativeRender();
        } catch (error) {
          recoveryErrors.push(error);
        }
        if (recoveryErrors.length > 0) {
          this.removeSink(itemId, sink);
          if (recoveryErrors.length === 1) throw recoveryErrors[0];
          throw new AggregateError(
            recoveryErrors,
            `streaming assistant reveal registration recovery failed for ${itemId}`,
          );
        }
      }
    }

    let registered = true;
    return () => {
      if (!registered) return;
      registered = false;
      try {
        sink.reset();
      } finally {
        this.removeSink(itemId, sink);
      }
    };
  }

  /**
   * Source passed to the markdown parser for the current canonical row.
   * Canonical row replacement is the reactive signal that re-reads this
   * plain Map. A mismatch is a caller bug, but recovering to the canonical
   * source is safer than leaving duplicated or truncated visible text.
   */
  parserSourceFor(
    itemId: string,
    canonicalSource: string,
    renderContext: StreamingAssistantRenderContext,
  ): string {
    const presentation = this.presentationByItem.get(itemId);
    if (!presentation) return canonicalSource;
    if (presentation.expectedSource !== canonicalSource) {
      const message = 'streaming assistant reveal canonical source changed outside the router';
      console.error(`[streaming-assistant-reveal] ${message}`, itemId);
      reportFrontendDiagnostic(message, `item=${itemId}`);
      this.resetForCanonicalRender(itemId);
      return canonicalSource;
    }
    if (presentation.renderContext === undefined) {
      presentation.renderContext = renderContext;
      return presentation.parserSource;
    }
    if (
      presentation.renderContext === renderContext ||
      sameRenderContext(presentation.renderContext, renderContext)
    ) {
      return presentation.parserSource;
    }
    this.resetForCanonicalRender(itemId);
    return canonicalSource;
  }

  /**
   * Guard every pane-owned item replacement. Direct reveals update the expected
   * source before committing, so they pass. Any other summary or markdown-meta
   * change drops the masked parser source before the row signal fires.
   */
  reconcileItemWrite(
    previous: { id: string; kind: string; summary: string; meta?: string },
    next: { id: string; kind: string; summary: string; meta?: string },
  ): void {
    if (previous.id !== next.id) {
      throw new Error(
        `streaming assistant reveal cannot reconcile ${previous.id} as ${next.id}`,
      );
    }
    const presentation = this.presentationByItem.get(next.id);
    if (!presentation) return;
    if (
      next.kind !== 'assistant_text' ||
      next.summary !== presentation.expectedSource ||
      next.meta !== previous.meta
    ) {
      this.resetItem(next.id);
    }
  }

  /**
   * Append one literal reveal unit while keeping the markdown parser at its
   * last authoritative source. This method owns the canonical write for every
   * outcome. A caller cannot accidentally advance sink-owned DOM without the
   * row, or arm a parser checkpoint without publishing the source it describes.
   * The commit mode tells the pane whether to replace the row reactively or
   * update its raw canonical object quietly after every sink passed preflight.
   */
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
  ): boolean {
    if (delta.length === 0) {
      throw new Error('streaming assistant reveal cannot publish an empty delta');
    }
    const append = createProvenAppend(source, delta);
    const nextSource = append.next;
    const presentation = this.presentationByItem.get(itemId);
    const sinks = this.sinksByItem.get(itemId);
    if (
      !isSafeAssistantLiteralDelta(delta) ||
      !isSafeAssistantLiteralPredecessor(previousCodeUnit) ||
      (presentation !== undefined && presentation.expectedSource !== source) ||
      !sinks ||
      sinks.size === 0
    ) {
      const checkpoint = sinks && sinks.size > 0
        ? checkpointFromDelta(delta)
        : undefined;
      if (presentation) {
        return this.commitAuthoritativeAfterReset(
          itemId,
          nextSource,
          append,
          commitCanonical,
          checkpoint ?? false,
          'reset',
        );
      }
      if (checkpoint) {
        this.presentationByItem.set(
          itemId,
          createDirectPresentation(nextSource, checkpoint),
        );
      }
      try {
        commitCanonical(nextSource, 'authoritative', append);
      } catch (error) {
        this.throwAfterReset(itemId, error, 'checkpoint commit');
      }
      return false;
    }

    // A full parser render must establish the trailing leaf before the first
    // direct append in each literal run. Markdown such as `# ` and `> ` has a
    // text-looking placeholder before its first word, but that word changes
    // the element tree. An authoritative unit arms a bounded literal-tail
    // checkpoint above whenever it has one. If it had no literal tail, this
    // first safe unit pays the authoritative render and arms the checkpoint.
    if (!presentation) {
      const checkpoint = checkpointFromDelta(delta);
      if (checkpoint) {
        this.presentationByItem.set(
          itemId,
          createDirectPresentation(nextSource, checkpoint),
        );
      }
      try {
        commitCanonical(nextSource, 'authoritative', append);
      } catch (error) {
        this.throwAfterReset(itemId, error, 'checkpoint commit');
      }
      return false;
    }

    try {
      for (const sink of sinks) {
        if (!sink.canAppendLiteral(
          source,
          presentation.parserCheckpoint,
          nextSource,
          delta,
        )) {
          return this.commitAuthoritativeAfterReset(
            itemId,
            nextSource,
            append,
            commitCanonical,
            advanceCheckpoint(presentation.expectedCheckpoint, delta),
            'reset',
          );
        }
      }
    } catch (error) {
      return this.recoverSinkFailure(
        itemId,
        nextSource,
        append,
        commitCanonical,
        error,
        'preflight',
      );
    }

    presentation.expectedSource = nextSource;
    presentation.directDeltas.push(delta);
    advanceCheckpointInPlace(presentation.expectedCheckpoint, delta);
    try {
      commitCanonical(nextSource, 'direct', append);
    } catch (error) {
      this.throwAfterReset(itemId, error, 'commit');
    }
    try {
      for (const sink of sinks) sink.appendLiteral(nextSource, delta);
    } catch (error) {
      return this.recoverSinkFailure(
        itemId,
        nextSource,
        append,
        commitCanonical,
        error,
        'append',
      );
    }
    return true;
  }

  clearItem(itemId: string): void {
    this.resetItem(itemId);
  }

  /**
   * Retire sink-owned DOM and publish the complete canonical source through
   * the caller's reactive row write. Clearing a live presentation without the
   * write would expose the older parser checkpoint until another wire event
   * happened to repaint the row.
   */
  retireItem(
    itemId: string,
    requestAuthoritativeRender: () => void,
  ): void {
    const needsAuthoritativeRender = this.presentationByItem.has(itemId);
    const errors: unknown[] = [];
    try {
      this.resetItem(itemId);
    } catch (error) {
      errors.push(error);
    }
    if (needsAuthoritativeRender) {
      try {
        requestAuthoritativeRender();
      } catch (error) {
        errors.push(error);
      }
    }
    if (errors.length === 1) throw errors[0];
    if (errors.length > 1) {
      throw new AggregateError(
        errors,
        `streaming assistant reveal retirement failed for ${itemId}`,
      );
    }
  }

  dispose(): void {
    const errors: unknown[] = [];
    for (const sinks of this.sinksByItem.values()) {
      for (const sink of sinks) {
        try {
          sink.reset();
        } catch (error) {
          errors.push(error);
        }
      }
    }
    this.sinksByItem.clear();
    this.presentationByItem.clear();
    if (errors.length > 0) {
      throw new AggregateError(errors, 'streaming assistant reveal disposal failed');
    }
  }

  private resetItem(itemId: string): void {
    try {
      this.resetSinkDom(itemId);
    } finally {
      this.presentationByItem.delete(itemId);
    }
  }

  private resetForCanonicalRender(itemId: string): void {
    try {
      this.resetItem(itemId);
    } catch (error) {
      this.reportRecoveredSinkFailure(itemId, 'reset', error);
    }
  }

  private resetSinkDom(itemId: string): void {
    const sinks = this.sinksByItem.get(itemId);
    if (!sinks) return;
    const errors: unknown[] = [];
    for (const sink of sinks) {
      try {
        sink.reset();
      } catch (error) {
        errors.push(error);
      }
    }
    if (errors.length > 0) {
      throw new AggregateError(errors, `streaming assistant reveal reset failed for ${itemId}`);
    }
  }

  private removeSink(itemId: string, sink: StreamingAssistantRevealSink): void {
    const sinks = this.sinksByItem.get(itemId);
    sinks?.delete(sink);
    if (sinks?.size === 0) {
      this.sinksByItem.delete(itemId);
      this.presentationByItem.delete(itemId);
    }
  }

  private throwAfterReset(itemId: string, error: unknown, phase: string): never {
    try {
      this.resetItem(itemId);
    } catch (resetError) {
      throw new AggregateError(
        [error, resetError],
        `streaming assistant reveal ${phase} and reset failed for ${itemId}`,
      );
    }
    throw error;
  }

  /**
   * Drop sink-owned DOM, then publish the complete source through Svelte. A
   * reset callback belongs to the optional direct-render path. If it fails,
   * the authoritative row replacement is still the recovery mechanism and
   * the smoother must keep advancing. Only a failed authoritative commit can
   * leave the transcript unrepaired, so that combination remains fatal.
   */
  private commitAuthoritativeAfterReset(
    itemId: string,
    nextSource: string,
    append: ProvenAppend,
    commitCanonical: (
      source: string,
      mode: StreamingAssistantCommitMode,
      append: ProvenAppend,
    ) => void,
    checkpoint: StreamingAssistantParserCheckpoint | false,
    phase: 'reset',
  ): false {
    let resetFailure: unknown;
    try {
      this.resetItem(itemId);
    } catch (error) {
      resetFailure = error;
    }

    if (resetFailure === undefined && checkpoint) {
      this.presentationByItem.set(
        itemId,
        createDirectPresentation(nextSource, checkpoint),
      );
    }
    try {
      commitCanonical(nextSource, 'authoritative', append);
    } catch (commitFailure) {
      if (resetFailure !== undefined) {
        throw new AggregateError(
          [resetFailure, commitFailure],
          `streaming assistant reveal ${phase} recovery failed for ${itemId}`,
        );
      }
      this.throwAfterReset(itemId, commitFailure, 'fallback commit');
    }

    if (resetFailure !== undefined) {
      this.reportRecoveredSinkFailure(itemId, phase, resetFailure);
    }
    return false;
  }

  /**
   * A mounted DOM sink is an optimization boundary, not transcript state. If
   * it fails, restore the canonical row and keep the smoother moving. The
   * always-on diagnostic makes the degradation visible without turning a
   * repaired presentation failure into skipped text or a wedged reveal gate.
   */
  private recoverSinkFailure(
    itemId: string,
    nextSource: string,
    append: ProvenAppend,
    commitCanonical: (
      source: string,
      mode: StreamingAssistantCommitMode,
      append: ProvenAppend,
    ) => void,
    failure: unknown,
    phase: 'preflight' | 'append',
  ): false {
    const recoveryErrors: unknown[] = [];
    try {
      this.resetItem(itemId);
    } catch (error) {
      recoveryErrors.push(error);
    }
    // The append phase already landed its quiet write. Replacing the row with
    // the same source is intentional because it hands the whole source back
    // to Svelte after the direct checkpoint was dropped.
    try {
      commitCanonical(nextSource, 'authoritative', append);
    } catch (error) {
      recoveryErrors.push(error);
    }
    if (recoveryErrors.length > 0) {
      throw new AggregateError(
        [failure, ...recoveryErrors],
        `streaming assistant reveal ${phase} recovery failed for ${itemId}`,
      );
    }

    this.reportRecoveredSinkFailure(itemId, phase, failure);
    return false;
  }

  private reportRecoveredSinkFailure(
    itemId: string,
    phase: 'preflight' | 'append' | 'reset',
    failure: unknown,
  ): void {
    const failureText = failure instanceof Error
      ? `${failure.name}: ${failure.message}`
      : String(failure);
    const message = 'streaming assistant direct reveal fell back after a sink failure';
    const detail = `item=${itemId}; phase=${phase}; error=${failureText}`;
    console.warn(`[streaming-assistant-reveal] ${message}`, detail);
    reportFrontendDiagnostic(message, detail);
  }
}

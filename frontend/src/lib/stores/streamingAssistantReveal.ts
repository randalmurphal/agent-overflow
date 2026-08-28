export interface StreamingAssistantRevealSink {
  /**
   * Verify that this mounted representation still renders `source` at a
   * literal text leaf that can accept `delta` without changing markdown
   * structure. This is a read-only preflight. The router calls every sink
   * before it mutates the canonical item or any DOM.
   */
  canAppendLiteral(source: string, nextSource: string, delta: string): boolean;
  /** Append a delta that already passed every sink's preflight. */
  appendLiteral(nextSource: string, delta: string): void;
  /** Remove DOM owned by the direct path before an authoritative render. */
  reset(): void;
}

const LITERAL_TEXT = /^[\p{L}\p{M}\p{N} ]+$/u;

function isSafePreviousCodeUnit(code: number): boolean {
  return code === -1 || code === 32 ||
    (code >= 48 && code <= 57) ||
    (code >= 65 && code <= 90) ||
    (code >= 97 && code <= 122) ||
    code >= 128;
}

/**
 * One reveal router belongs to one thread-pane store. Item ids are globally
 * stable, so a process-global registry would route duplicate projections
 * through each other and append every reveal twice.
 *
 * The router owns the two-phase commit. It preflights every mounted
 * representation, advances the canonical item once, then appends to every
 * DOM sink. Any unsafe delta resets all sink-owned DOM and returns false so
 * the caller performs the ordinary authoritative markdown render.
 */
export class StreamingAssistantRevealRouter {
  private readonly sinksByItem = new Map<string, Set<StreamingAssistantRevealSink>>();
  private readonly expectedSourceByItem = new Map<string, string>();
  private readonly armedItems = new Set<string>();

  register(itemId: string, sink: StreamingAssistantRevealSink): () => void {
    let sinks = this.sinksByItem.get(itemId);
    if (!sinks) {
      sinks = new Set();
      this.sinksByItem.set(itemId, sinks);
    }
    sinks.add(sink);

    let registered = true;
    return () => {
      if (!registered) return;
      registered = false;
      sink.reset();
      sinks?.delete(sink);
      if (sinks?.size === 0) {
        this.sinksByItem.delete(itemId);
        this.expectedSourceByItem.delete(itemId);
        this.armedItems.delete(itemId);
      }
    };
  }

  /**
   * Append one literal reveal unit without invalidating the Svelte row.
   * `commitCanonical` runs only after every sink has proved it can append.
   */
  publish(
    itemId: string,
    previousCodeUnit: number,
    source: string,
    delta: string,
    commitCanonical: (nextSource: string) => void,
  ): boolean {
    const nextSource = source + delta;
    const expectedSource = this.expectedSourceByItem.get(itemId);
    const sinks = this.sinksByItem.get(itemId);
    if (
      delta.length === 0 ||
      !LITERAL_TEXT.test(delta) ||
      !isSafePreviousCodeUnit(previousCodeUnit) ||
      (expectedSource !== undefined && expectedSource !== source) ||
      !sinks ||
      sinks.size === 0
    ) {
      this.resetItem(itemId);
      return false;
    }

    // A full parser render must establish the trailing leaf before the first
    // direct append in each literal run. Markdown such as `# ` and `> ` has a
    // text-looking placeholder before its first word, but that word changes
    // the element tree. The first safe unit pays the authoritative render.
    if (!this.armedItems.has(itemId)) {
      this.resetSinkDom(itemId);
      this.armedItems.add(itemId);
      this.expectedSourceByItem.set(itemId, nextSource);
      return false;
    }

    for (const sink of sinks) {
      if (!sink.canAppendLiteral(source, nextSource, delta)) {
        this.resetSinkDom(itemId);
        this.armedItems.add(itemId);
        this.expectedSourceByItem.set(itemId, nextSource);
        return false;
      }
    }

    commitCanonical(nextSource);
    try {
      for (const sink of sinks) sink.appendLiteral(nextSource, delta);
    } catch (error) {
      this.resetItem(itemId);
      throw error;
    }
    this.expectedSourceByItem.set(itemId, nextSource);
    return true;
  }

  clearItem(itemId: string): void {
    this.resetItem(itemId);
  }

  dispose(): void {
    for (const sinks of this.sinksByItem.values()) {
      for (const sink of sinks) sink.reset();
    }
    this.sinksByItem.clear();
    this.expectedSourceByItem.clear();
    this.armedItems.clear();
  }

  private resetItem(itemId: string): void {
    this.resetSinkDom(itemId);
    this.expectedSourceByItem.delete(itemId);
    this.armedItems.delete(itemId);
  }

  private resetSinkDom(itemId: string): void {
    const sinks = this.sinksByItem.get(itemId);
    if (sinks) {
      for (const sink of sinks) sink.reset();
    }
  }
}

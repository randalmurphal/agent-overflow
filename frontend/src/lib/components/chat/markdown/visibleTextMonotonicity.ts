/**
 * Mutation-record-level reconstruction of an element subtree's visible text.
 *
 * `textContent` read after a batch of mutations cannot see an intermediate
 * state, and that intermediate state is exactly the defect this instrument
 * exists for: the streaming reveal used to delete already-painted Text nodes
 * in one task and let Svelte re-extend the parser's node in a later one, so
 * the visible string shrank and regrew. Chromium coalesces that before the
 * next animation frame; WebView2 is allowed not to.
 *
 * The recorder therefore replays every `MutationRecord` in order against a
 * flat model of the subtree's Text nodes, and reports the visible string after
 * EACH record. A record that both adds and removes nodes (the `replace all`
 * algorithm behind `replaceChildren` / `textContent =`) is a single atomic
 * swap: no intermediate state exists between its two halves, so it is the one
 * legal way for the visible string to stop extending.
 *
 * Test-only; not imported by product code.
 */

export interface VisibleTextStep {
  /** Visible string after this record was applied. */
  readonly visible: string;
  /** Visible string before this record was applied. */
  readonly previous: string;
  /** `previous` is a prefix of `visible`. */
  readonly extends: boolean;
  /** The record replaced nodes in one `replace all` step. */
  readonly atomicSwap: boolean;
  readonly type: MutationRecordType;
  /** True when the record's insertion point could not be resolved exactly. */
  readonly unplaced: boolean;
}

export interface VisibleTextViolation extends VisibleTextStep {
  readonly index: number;
}

/**
 * A checkpoint is the LIVE `textContent` read at a microtask boundary — one
 * `MutationObserver` callback, i.e. everything one synchronous task mutated.
 * No model, no replay: this is what any other microtask, and therefore any
 * frame, could actually observe.
 *
 * It is the invariant that survives Streamdown's committed-block promotion,
 * which moves a settled paragraph out of the volatile tail by appending it to
 * the committed container and then removing it from the volatile one — two
 * records that transiently duplicate the text inside a single flush.
 *
 * `rollback` is the defect this lane exists to catch, and it is narrower than
 * "did not extend". A checkpoint that shrinks to a PREFIX of what was already
 * on screen has walked the reader backwards through text they had read — the
 * incident signature ("a long prefix dropped back to `The hour-long`"). A
 * checkpoint that shrinks to something that is NOT a prefix has replaced the
 * text with different content, which is the legal divergence: the provider's
 * final body genuinely differs from what the reveal painted, and the owner
 * swaps it in one atomic record.
 */
export interface VisibleTextCheckpoint {
  readonly visible: string;
  readonly previous: string;
  readonly extends: boolean;
  /** Shrank to a prefix of the text already shown. */
  readonly rollback: boolean;
  readonly records: number;
}

function textNodesOf(node: Node): Text[] {
  if (node.nodeType === Node.TEXT_NODE) return [node as Text];
  if (node.nodeType !== Node.ELEMENT_NODE && node.nodeType !== Node.DOCUMENT_FRAGMENT_NODE) {
    return [];
  }
  const out: Text[] = [];
  const walker = document.createTreeWalker(node, NodeFilter.SHOW_TEXT);
  for (let next = walker.nextNode(); next; next = walker.nextNode()) {
    out.push(next as Text);
  }
  return out;
}

/**
 * Records the visible text of `root` after every mutation record it produces.
 * Start recording before the first mutation; drain with `drain()` whenever the
 * caller needs the model current (the observer callback drains on its own, but
 * a synchronous assertion after an `await tick()` may run first).
 */
export function recordVisibleText(root: Element): {
  drain(): void;
  steps(): readonly VisibleTextStep[];
  violations(): readonly VisibleTextViolation[];
  checkpointViolations(): readonly VisibleTextViolation[];
  visible(): string;
  matchesDom(): boolean;
  stop(): void;
} {
  let nodes: Text[] = textNodesOf(root);
  const data = new Map<Text, string>();
  for (const node of nodes) data.set(node, node.data);
  const steps: VisibleTextStep[] = [];
  const checkpoints: VisibleTextCheckpoint[] = [];
  let lastCheckpoint = root.textContent ?? '';
  let visible = nodes.map((node) => data.get(node) ?? '').join('');

  function render(): string {
    let out = '';
    for (const node of nodes) out += data.get(node) ?? '';
    return out;
  }

  function indexAfter(reference: Node): number | null {
    const texts = textNodesOf(reference);
    for (let index = texts.length - 1; index >= 0; index--) {
      const at = nodes.indexOf(texts[index]);
      if (at >= 0) return at + 1;
    }
    return null;
  }

  function indexBefore(reference: Node): number | null {
    for (const text of textNodesOf(reference)) {
      const at = nodes.indexOf(text);
      if (at >= 0) return at;
    }
    return null;
  }

  /**
   * Document-order fallback for an insertion whose siblings carry no text the
   * model knows: place the new nodes before the first modelled node that
   * follows the mutation's parent in the live tree.
   */
  function indexByDocumentOrder(target: Node): number {
    for (let index = 0; index < nodes.length; index++) {
      const node = nodes[index];
      if (!node.isConnected) continue;
      const position = target.compareDocumentPosition(node);
      if (
        (position & Node.DOCUMENT_POSITION_FOLLOWING) !== 0 &&
        (position & Node.DOCUMENT_POSITION_CONTAINED_BY) === 0
      ) return index;
    }
    return nodes.length;
  }

  function apply(records: readonly MutationRecord[]): void {
    if (records.length === 0) return;
    // Every record in this batch is already applied to the live tree, so a
    // node's LIVE data is its value after the last record, never its value at
    // the record being replayed. A Text node can change several times in one
    // batch, so the value right after record i is the old value of the next
    // record on the same target, and the live value when none follows. Holding
    // the remaining old values per target makes that answer available to
    // childList replay too: a node added mid-batch must enter the model at the
    // value it had THEN, not at the one it ends the batch with.
    const upcomingOldValues = new Map<Node, string[]>();
    for (const record of records) {
      if (record.type !== 'characterData') continue;
      const queued = upcomingOldValues.get(record.target);
      if (queued) queued.push(record.oldValue ?? '');
      else upcomingOldValues.set(record.target, [record.oldValue ?? '']);
    }
    /** The node's value at the current point of the replay. */
    const valueNow = (node: Text): string => {
      const queued = upcomingOldValues.get(node);
      return queued && queued.length > 0 ? queued[0] : node.data;
    };

    for (const record of records) {
      const previous = visible;
      let unplaced = false;
      if (record.type === 'characterData') {
        const target = record.target as Text;
        const queued = upcomingOldValues.get(target);
        // Consume this record, so `valueNow` reports the value AFTER it.
        if (queued) queued.shift();
        const value = valueNow(target);
        if (data.has(target)) data.set(target, value);
        else unplaced = true;
      } else if (record.type === 'childList') {
        for (const removed of record.removedNodes) {
          for (const text of textNodesOf(removed)) {
            const at = nodes.indexOf(text);
            if (at >= 0) nodes.splice(at, 1);
            data.delete(text);
          }
        }
        // An element carries its whole subtree in ONE record — a block of
        // fixed-tag HTML emits nothing for its descendants — so an added
        // element must be expanded. But `textNodesOf` walks the LIVE subtree,
        // which by drain time also holds descendants that arrive under their
        // own later record in this same batch. Expanding and then honouring
        // that later record would count one Text node twice, which reads as
        // the visible string duplicating itself. The model owns each node
        // once: an add for a node already modelled is a no-op.
        const added: Text[] = [];
        for (const node of record.addedNodes) {
          for (const text of textNodesOf(node)) {
            if (data.has(text) || added.includes(text)) continue;
            added.push(text);
          }
        }
        if (added.length > 0) {
          let at: number | null = null;
          if (record.previousSibling) at = indexAfter(record.previousSibling);
          if (at === null && record.nextSibling) at = indexBefore(record.nextSibling);
          if (at === null) {
            at = indexAfter(record.target) ?? indexByDocumentOrder(record.target);
            unplaced = true;
          }
          for (const text of added) data.set(text, valueNow(text));
          nodes.splice(at, 0, ...added);
        }
      }
      visible = render();
      steps.push({
        visible,
        previous,
        extends: visible.startsWith(previous),
        atomicSwap:
          record.type === 'childList' &&
          record.addedNodes.length > 0 &&
          record.removedNodes.length > 0,
        type: record.type,
        unplaced,
      });
    }
    const live = root.textContent ?? '';
    const grew = live.startsWith(lastCheckpoint);
    checkpoints.push({
      visible: live,
      previous: lastCheckpoint,
      extends: grew,
      rollback: !grew && lastCheckpoint.startsWith(live),
      records: records.length,
    });
    lastCheckpoint = live;
  }

  const observer = new MutationObserver((records) => apply(records));
  observer.observe(root, {
    subtree: true,
    childList: true,
    characterData: true,
    characterDataOldValue: true,
  });

  return {
    drain() {
      apply(observer.takeRecords());
    },
    steps() {
      return steps;
    },
    violations() {
      const out: VisibleTextViolation[] = [];
      for (let index = 0; index < steps.length; index++) {
        const step = steps[index];
        if (step.extends || step.atomicSwap) continue;
        out.push({ ...step, index });
      }
      return out;
    },
    /**
     * Task-level rollbacks: a synchronous task that left the reader looking at
     * a PREFIX of what they could already see. See `VisibleTextCheckpoint` for
     * why a non-prefix shrink is the legal divergence rather than a defect.
     */
    checkpointViolations() {
      const out: VisibleTextViolation[] = [];
      for (let index = 0; index < checkpoints.length; index++) {
        const checkpoint = checkpoints[index];
        if (!checkpoint.rollback) continue;
        out.push({
          ...checkpoint,
          atomicSwap: false,
          type: 'childList',
          unplaced: false,
          index,
        });
      }
      return out;
    },
    visible() {
      return visible;
    },
    /**
     * Self-check: after a full drain the replayed model must equal the live
     * tree. A mismatch means a record could not be placed and the violation
     * list is describing the instrument, not the product.
     */
    matchesDom() {
      return visible === (root.textContent ?? '');
    },
    stop() {
      apply(observer.takeRecords());
      observer.disconnect();
    },
  };
}

export function describeViolations(
  violations: readonly VisibleTextViolation[],
): string {
  return violations
    .slice(0, 8)
    .map((violation) => {
      const kept = violation.visible.length;
      const lost = violation.previous.length - kept;
      return `#${violation.index} ${violation.type}: -${lost} code units, ` +
        `now …${JSON.stringify(violation.visible.slice(-48))} ` +
        `was …${JSON.stringify(violation.previous.slice(-48))}`;
    })
    .join('\n');
}

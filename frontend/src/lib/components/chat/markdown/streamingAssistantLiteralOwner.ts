import {
  streamdownLiteralHostOf,
  type StreamdownLiteralHost,
} from '../../../markdown';
import type { PathRef } from '../../../types/models';
import type {
  StreamingAssistantParserCheckpoint,
  StreamingAssistantRevealSink,
} from '../../../stores/streamingAssistantReveal';
import {
  captureStreamingAssistantSelection,
  restoreStreamingAssistantSelection,
} from './streamingAssistantSelection';

const DIRECT_TEXT_NODE_LIMIT = 256;

function rangeEndsWith(
  value: string,
  valueEnd: number,
  suffix: string,
  suffixStart: number,
  suffixEnd: number,
): boolean {
  const length = suffixEnd - suffixStart;
  if (length <= 0 || valueEnd < length) return false;
  const valueStart = valueEnd - length;
  for (let index = 0; index < length; index++) {
    if (value.charCodeAt(valueStart + index) !== suffix.charCodeAt(suffixStart + index)) {
      return false;
    }
  }
  return true;
}

function trailingAsciiSpaceCount(value: string): number {
  let index = value.length;
  while (index > 0 && value.charCodeAt(index - 1) === 32) index--;
  return value.length - index;
}

let textRunSegmenter: Intl.Segmenter | undefined;

function directTextRunSegmenter(): Intl.Segmenter {
  textRunSegmenter ??= new Intl.Segmenter('und', { granularity: 'word' });
  return textRunSegmenter;
}

/**
 * Finds a Unicode word boundary for the next bounded Text node write. `prefix`
 * is the current node's data. Including it lets a later delta extend a shaping
 * run that ended exactly at the prior size limit, including combining marks,
 * emoji joiners, regional indicators, and cursive-script words. One run may
 * exceed the nominal limit; visual text integrity takes precedence over the
 * allocation bound.
 */
function textRunSafeChunkEnd(
  value: string,
  start: number,
  prefix: string,
  maxNodeCodeUnits: number,
): number {
  const remaining = value.slice(start);
  const combined = prefix + remaining;
  const prefixLength = prefix.length;
  let prefixIsBoundary = prefixLength === 0;
  let lastFittingBoundary = -1;
  let firstFollowingBoundary = -1;

  const visitBoundary = (boundary: number): void => {
    if (boundary === prefixLength) prefixIsBoundary = true;
    if (boundary <= prefixLength) return;
    if (firstFollowingBoundary < 0) firstFollowingBoundary = boundary;
    if (boundary <= maxNodeCodeUnits) lastFittingBoundary = boundary;
  };
  for (const segment of directTextRunSegmenter().segment(combined)) {
    visitBoundary(segment.index);
  }
  visitBoundary(combined.length);

  if (lastFittingBoundary > prefixLength) {
    return start + lastFittingBoundary - prefixLength;
  }
  if (prefixLength > 0 && prefixIsBoundary) {
    // The next whole cluster does not fit this node. Start it in a new one.
    return start;
  }
  if (firstFollowingBoundary > prefixLength) {
    // The current node ends inside a cluster, or the next cluster alone is
    // larger than the limit. Keep that cluster whole even if this node grows.
    return start + firstFollowingBoundary - prefixLength;
  }
  return value.length;
}

/**
 * Detects allowlisted paths completed by a direct delta without searching the
 * growing canonical string. Only the longest possible crossing prefix is
 * retained. After the first preflight, every search runs over that bounded
 * tail plus the new reveal unit.
 */
export class AllowlistedPathCompletionGuard {
  private pathRefs: readonly PathRef[] | undefined;
  private expectedSource: string | undefined;
  private sourceTail = '';
  private maxPrefixCharacters = 0;

  completes(
    pathRefs: readonly PathRef[],
    source: string,
    nextSource: string,
    delta: string,
  ): boolean {
    if (pathRefs.length === 0) {
      this.pathRefs = pathRefs;
      this.expectedSource = nextSource;
      this.sourceTail = '';
      this.maxPrefixCharacters = 0;
      return false;
    }

    if (this.pathRefs !== pathRefs) {
      this.pathRefs = pathRefs;
      this.expectedSource = undefined;
      this.maxPrefixCharacters = 0;
      for (let index = 0; index < pathRefs.length; index++) {
        this.maxPrefixCharacters = Math.max(
          this.maxPrefixCharacters,
          Math.max(0, pathRefs[index].path.length - 1),
        );
      }
    }

    if (this.expectedSource !== source) {
      this.sourceTail = source.slice(
        Math.max(0, source.length - this.maxPrefixCharacters),
      );
    }

    const oldTailLength = this.sourceTail.length;
    const searchWindow = this.sourceTail + delta;
    let completed = false;
    for (let index = 0; index < pathRefs.length && !completed; index++) {
      const path = pathRefs[index].path;
      if (path.length === 0) continue;
      let offset = Math.max(0, oldTailLength - path.length + 1);
      while ((offset = searchWindow.indexOf(path, offset)) !== -1) {
        if (offset + path.length > oldTailLength) {
          completed = true;
          break;
        }
        offset += 1;
      }
    }

    this.sourceTail = searchWindow.slice(
      Math.max(0, searchWindow.length - this.maxPrefixCharacters),
    );
    this.expectedSource = nextSource;
    return completed;
  }
}

interface StreamingAssistantLiteralOwnerOptions {
  getRoot(): HTMLElement | undefined;
  canAppendSource(source: string, nextSource: string, delta: string): boolean;
}

/**
 * The single owner of the active literal host's visible text.
 *
 * The host (vendored `LiteralHost.svelte`, divergence 21) renders no Text node
 * of its own. This owner adopts it and is the only writer of its children, so
 * one visible text run is never split between Svelte and the reveal. Two
 * writers is what produced the settle flicker: `reset()` deleted the revealed
 * suffix in the publish task and Svelte re-extended the parser's node in a
 * later one, so the DOM rolled back to an older parser checkpoint in between.
 *
 * The invariant here is ownership, not timing:
 *
 *  - a reveal delta EXTENDS the visible string, in bounded Text nodes;
 *  - an authoritative parser update either extends it too (the ordinary
 *    punctuation/structure fallback, where the parser has simply caught up
 *    with what the reveal already painted) or, when the upstream text genuinely
 *    diverges, replaces it in ONE `replaceChildren` mutation record;
 *  - `reset()` relinquishes the RUN and never deletes visible bytes.
 *
 * Do not normalize the bounded nodes together. Svelte keeps a private cached
 * value on every reactive Text node, and `Node.normalize()` changes nodeValue
 * without changing that cache — the reason this host renders empty rather than
 * letting Svelte own a node inside it at all.
 */
export function createStreamingAssistantLiteralOwner(
  options: StreamingAssistantLiteralOwnerOptions,
): StreamingAssistantRevealSink {
  let hostElement: HTMLElement | null = null;
  let releaseHost: (() => void) | null = null;
  /** Every Text node in the host, in document order. */
  let nodes: Text[] = [];
  /** The bounded node still open for appends. */
  let activeNode: Text | null = null;
  /** Exactly the concatenation of `nodes`. Replaces the old base-data copy. */
  let visible = '';
  /** Canonical source `visible` represents while a run is live. */
  let runSource = '';
  let runActive = false;
  let pendingWhitespaceLength = 0;
  let selectionRestoreGeneration = 0;

  function findHostElement(): HTMLElement | null {
    const root = options.getRoot();
    if (!root) return null;
    const hosts = root.querySelectorAll<HTMLElement>(
      '.md-volatile [data-streamdown-direct-append-safe]',
    );
    return hosts.item(hosts.length - 1) || null;
  }

  function forgetHost(): void {
    const release = releaseHost;
    releaseHost = null;
    hostElement = null;
    nodes = [];
    activeNode = null;
    visible = '';
    runSource = '';
    runActive = false;
    pendingWhitespaceLength = 0;
    release?.();
  }

  /** The controller handed the element to someone else, or unmounted it. */
  function onReleased(): void {
    releaseHost = null;
    forgetHost();
  }

  /**
   * Adopt whatever the renderer currently presents. Adoption mutates nothing:
   * the owner inherits the on-screen text and extends from there.
   */
  function ensureHost(): boolean {
    const found = findHostElement();
    if (!found) {
      if (hostElement) forgetHost();
      return false;
    }
    if (found === hostElement && releaseHost) return true;
    forgetHost();
    const handle: StreamdownLiteralHost | null = streamdownLiteralHostOf(found);
    if (!handle) return false;
    const adopted: Text[] = [];
    let adoptedText = '';
    for (const node of found.childNodes) {
      if (!(node instanceof Text)) return false;
      adopted.push(node);
      adoptedText += node.data;
    }
    hostElement = found;
    nodes = adopted;
    visible = adoptedText;
    releaseHost = handle.adopt({ present, release: onReleased });
    return true;
  }

  /**
   * Re-derive the owner's bookkeeping from the live host. Returns false when
   * the host holds something this owner cannot append after, which leaves an
   * atomic replacement as the only correct presentation.
   */
  function resyncFromDom(host: HTMLElement): boolean {
    const found: Text[] = [];
    let foundText = '';
    let pureText = true;
    for (const node of host.childNodes) {
      if (node instanceof Text) {
        found.push(node);
        foundText += node.data;
      } else pureText = false;
    }
    nodes = found;
    visible = foundText;
    activeNode = null;
    pendingWhitespaceLength = 0;
    runActive = false;
    runSource = '';
    return pureText;
  }

  function domIsConsistent(): boolean {
    const root = options.getRoot();
    const host = hostElement;
    if (
      !root ||
      !host ||
      !releaseHost ||
      !host.isConnected ||
      !root.contains(host) ||
      host.childNodes.length !== nodes.length
    ) return false;
    let length = 0;
    for (let index = 0; index < nodes.length; index++) {
      const node = nodes[index];
      if (host.childNodes.item(index) !== node) return false;
      length += node.length;
    }
    return length === visible.length;
  }

  function adoptRun(
    source: string,
    checkpoint: StreamingAssistantParserCheckpoint,
  ): boolean {
    if (!ensureHost()) return false;
    if (!domIsConsistent()) {
      // Something outside this owner changed the host. Re-derive what is
      // visible instead of trusting stale bookkeeping — and, deliberately,
      // without deleting anything.
      forgetHost();
      if (!ensureHost() || !domIsConsistent()) return false;
    }
    if (runActive && runSource === source) return true;
    runActive = false;
    runSource = '';
    // parseIncompleteMarkdown may synthesize punctuation after the source's
    // live text. Most closers become an outer token boundary, where appending
    // inside the marked leaf is correct. Some extensions keep the synthetic
    // byte in the same text token (an unfinished description detail adds a
    // trailing colon). Appending after that byte visibly transposes the next
    // character. The host is safe only when its text still ends at the
    // canonical literal tail.
    const visibleTrailingWhitespace = trailingAsciiSpaceCount(visible);
    const visibleEnd = visible.length - visibleTrailingWhitespace;
    if (
      visibleTrailingWhitespace > checkpoint.trailingAsciiSpaces ||
      !rangeEndsWith(
        visible,
        visibleEnd,
        checkpoint.tailSource,
        checkpoint.tailStart,
        checkpoint.tailEnd,
      )
    ) return false;
    pendingWhitespaceLength = Math.max(
      0,
      checkpoint.trailingAsciiSpaces - visibleTrailingWhitespace,
    );
    runSource = source;
    runActive = true;
    return true;
  }

  function appendVisibleText(delta: string): void {
    const host = hostElement;
    if (!host) throw new Error('direct assistant reveal lost its text host');
    visible += delta;
    let offset = 0;
    while (offset < delta.length) {
      if (activeNode) {
        const remainingLength = delta.length - offset;
        const room = DIRECT_TEXT_NODE_LIMIT - activeNode.length;
        if (room >= remainingLength) {
          activeNode.appendData(delta.slice(offset));
          return;
        }
        const end = textRunSafeChunkEnd(
          delta,
          offset,
          activeNode.data,
          DIRECT_TEXT_NODE_LIMIT,
        );
        if (end === offset) {
          activeNode = null;
          continue;
        }
        const part = delta.slice(offset, end);
        activeNode.appendData(part);
        offset += part.length;
        continue;
      }
      if (delta.length - offset <= DIRECT_TEXT_NODE_LIMIT) {
        activeNode = document.createTextNode(delta.slice(offset));
        nodes.push(activeNode);
        host.append(activeNode);
        return;
      }
      const end = textRunSafeChunkEnd(
        delta,
        offset,
        '',
        DIRECT_TEXT_NODE_LIMIT,
      );
      const part = delta.slice(offset, end);
      activeNode = document.createTextNode(part);
      nodes.push(activeNode);
      host.append(activeNode);
      offset += part.length;
    }
  }

  function appendSource(delta: string): void {
    const trailingLength = trailingAsciiSpaceCount(delta);
    const visibleLength = delta.length - trailingLength;
    if (visibleLength === 0) {
      pendingWhitespaceLength += trailingLength;
      return;
    }
    if (pendingWhitespaceLength > 0) {
      appendVisibleText(' '.repeat(pendingWhitespaceLength));
    }
    appendVisibleText(delta.slice(0, visibleLength));
    pendingWhitespaceLength = trailingLength;
  }

  /**
   * The upstream text is not a continuation of what is on screen. Swap it in
   * one `replace all` mutation record so no reader — microtask, observer, or
   * compositor frame — can see between the removal and the insertion.
   */
  function replaceVisibleText(text: string, trailingWhitespace: number): void {
    const host = hostElement;
    if (!host) return;
    const root = options.getRoot();
    const selection = root ? captureStreamingAssistantSelection(root) : null;
    const next = document.createTextNode(text);
    host.replaceChildren(next);
    nodes = [next];
    // The replacement node carries the whole authoritative leaf; the next
    // reveal delta opens a fresh bounded node after it.
    activeNode = null;
    visible = text;
    pendingWhitespaceLength = trailingWhitespace;
    relinquishRun();
    if (selection) restoreStreamingAssistantSelection(selection);
  }

  /**
   * Relinquish the direct RUN. Nothing is removed: the bytes on screen are the
   * reveal's own output and the authoritative render that follows reconciles
   * against them through `present`. Deleting them here — ahead of a Svelte
   * flush that had not happened yet — is the rollback this owner exists to
   * remove.
   */
  function relinquishRun(): void {
    runActive = false;
    runSource = '';
  }

  /**
   * The router's fallback: relinquish the RUN and let an authoritative render
   * land. Nothing is removed — see the returned `reset`.
   *
   * The selection dance is the one thing that must survive it. This owner
   * never destroys a Text node the reader has selected, but the render that
   * follows can: a closing marker turns a literal leaf into an `<em>`, and
   * Svelte rebuilds that whole subtree, host included. So capture before the
   * flush and repair after it. `restoreStreamingAssistantSelection` no-ops
   * when the live selection already matches the snapshot, which is the
   * ordinary case here (an extending update touches no existing node), so the
   * repair costs nothing when nothing broke — and nothing at all is allocated
   * unless the reader actually has a selection in this body.
   */
  function relinquishForAuthoritativeRender(): void {
    const root = options.getRoot();
    const selection = root ? captureStreamingAssistantSelection(root) : null;
    relinquishRun();
    if (!selection) return;
    const generation = ++selectionRestoreGeneration;
    // The first microtask lets the canonical row write schedule Svelte's
    // flush; the second runs after that flush has rebuilt the leaf.
    queueMicrotask(() => queueMicrotask(() => {
      if (
        generation === selectionRestoreGeneration &&
        selection.root.isConnected &&
        options.getRoot()?.contains(selection.root)
      ) restoreStreamingAssistantSelection(selection);
    }));
  }

  /**
   * An authoritative parser update for the adopted host. Called by the vendored
   * controller inside the Svelte flush that publishes it.
   */
  function present(text: string): void {
    const host = hostElement;
    if (!host) return;
    const trailingWhitespace = trailingAsciiSpaceCount(text);
    const core = trailingWhitespace === 0
      ? text
      : text.slice(0, text.length - trailingWhitespace);
    // The owner must always leave the host presenting `text`; the renderer has
    // already recorded this update and will not publish it again.
    if (!domIsConsistent() && !resyncFromDom(host)) {
      replaceVisibleText(core, trailingWhitespace);
      return;
    }
    if (core === visible) {
      // The parser caught up with exactly what the reveal already painted.
      pendingWhitespaceLength = trailingWhitespace;
      relinquishRun();
      return;
    }
    if (core.length > visible.length && core.startsWith(visible)) {
      const previousPending = pendingWhitespaceLength;
      // The remainder already carries any whitespace the reveal deferred.
      pendingWhitespaceLength = 0;
      try {
        appendVisibleText(core.slice(visible.length));
      } catch (error) {
        pendingWhitespaceLength = previousPending;
        throw error;
      }
      pendingWhitespaceLength = trailingWhitespace;
      relinquishRun();
      return;
    }
    replaceVisibleText(core, trailingWhitespace);
  }

  return {
    canAppendLiteral(source, checkpoint, nextSource, delta) {
      return options.canAppendSource(source, nextSource, delta) &&
        adoptRun(source, checkpoint);
    },
    appendLiteral(nextSource, delta) {
      if (!runActive || !domIsConsistent()) {
        throw new Error('direct assistant reveal text host changed after preflight');
      }
      appendSource(delta);
      runSource = nextSource;
    },
    restoreLiteral(parserSource, checkpoint, source, directDeltas) {
      let expectedLength = parserSource.length;
      for (const delta of directDeltas) expectedLength += delta.length;
      if (expectedLength !== source.length || !adoptRun(parserSource, checkpoint)) {
        // A refused restore is a fallback like any other: the caller renders
        // authoritatively next, so the selection needs the same repair.
        relinquishForAuthoritativeRender();
        return false;
      }
      // The router stores only units that every mounted representation already
      // admitted under the same render context. Replaying those exact units is
      // the proof. Reconstructing one suffix from `source` would flatten the
      // canonical cons rope this path exists to preserve.
      for (const delta of directDeltas) appendSource(delta);
      runSource = source;
      return true;
    },
    /**
     * Relinquish the direct RUN. Nothing is removed: the bytes on screen are
     * the reveal's own output and the authoritative render that follows
     * reconciles against them through `present`. Deleting them here — ahead of
     * a Svelte flush that had not happened yet — is the rollback this owner
     * exists to remove.
     */
    reset: relinquishForAuthoritativeRender,
  };
}

import type { PathRef } from '../../../types/models';
import type {
  StreamingAssistantParserCheckpoint,
  StreamingAssistantRevealSink,
} from '../../../stores/streamingAssistantReveal';
import {
  captureStreamingAssistantSelection,
  restoreStreamingAssistantSelection,
  type StreamingAssistantSelectionSnapshot,
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

interface StreamingAssistantDomSinkOptions {
  getRoot(): HTMLElement | undefined;
  canAppendSource(source: string, nextSource: string, delta: string): boolean;
}

/**
 * Owns the DOM bytes appended between authoritative markdown renders.
 * The marker host contains one Svelte-owned Text node. Direct bytes live in
 * separate bounded Text nodes after it. A reset captures any selection, removes
 * only the sink-owned nodes, then lets the authoritative render update Svelte's
 * Text node before restoring the selection.
 *
 * Do not normalize the nodes into Svelte's Text node. Svelte keeps a private
 * cached value on every reactive Text node. Node.normalize() changes nodeValue
 * without changing that cache, so a later render whose intended leaf text still
 * equals the cached value skips its write and leaves the direct suffix in the
 * wrong Markdown leaf until another update happens.
 */
export function createStreamingAssistantDomSink(
  options: StreamingAssistantDomSinkOptions,
): StreamingAssistantRevealSink {
  type StreamingTextHost = HTMLSpanElement;
  let directHost: StreamingTextHost | null = null;
  let directBaseText: Text | null = null;
  let directBaseData = '';
  let directSource = '';
  let directTextNodes: Text[] = [];
  let activeDirectText: Text | null = null;
  let pendingWhitespaceLength = 0;
  let selectionRestoreGeneration = 0;

  function findDirectHost(): StreamingTextHost | null {
    const root = options.getRoot();
    if (!root) return null;
    const hosts = root.querySelectorAll<StreamingTextHost>(
      '.md-volatile [data-streamdown-direct-append-safe]',
    );
    return hosts.item(hosts.length - 1) || null;
  }

  function reset(): void {
    const restoreGeneration = ++selectionRestoreGeneration;
    const ownedNodes = directTextNodes;
    const host = directHost;
    const baseText = directBaseText;
    const baseData = directBaseData;
    const errors: unknown[] = [];
    let removedConsistently = false;
    let selection: StreamingAssistantSelectionSnapshot | null = null;
    try {
      let consistent = false;
      try {
        consistent = ownedNodes.length > 0 && host !== null && domIsConsistent(directSource);
      } catch (error) {
        errors.push(error);
      }
      if (consistent && host) {
        try {
          const root = options.getRoot();
          selection = root ? captureStreamingAssistantSelection(root) : null;
          for (const node of ownedNodes) node.remove();
          removedConsistently = true;
        } catch (error) {
          errors.push(error);
        }
      }
      if (!removedConsistently) {
        // Restore the parser-owned checkpoint even if a host mutation partially
        // changed the base, then remove every direct node. The authoritative
        // render that follows can now repair the row without duplicating a
        // suffix Svelte does not own.
        if (host && baseText?.parentNode === host) {
          try {
            baseText.data = baseData;
          } catch (error) {
            errors.push(error);
          }
        }
        for (const node of ownedNodes) {
          try {
            node.parentNode?.removeChild(node);
          } catch (error) {
            errors.push(error);
          }
        }
      }
    } finally {
      directHost = null;
      directBaseText = null;
      directBaseData = '';
      directSource = '';
      directTextNodes = [];
      activeDirectText = null;
      pendingWhitespaceLength = 0;
    }
    if (removedConsistently && selection) {
      // reset runs before the canonical row write on the ordinary fallback
      // path. The first microtask lets that write schedule Svelte's flush;
      // the second runs after the flush has replaced the Text node's data.
      queueMicrotask(() => queueMicrotask(() => {
        if (
          restoreGeneration === selectionRestoreGeneration &&
          selection.root.isConnected &&
          options.getRoot()?.contains(selection.root)
        ) restoreStreamingAssistantSelection(selection);
      }));
    }
    if (errors.length === 1) throw errors[0];
    if (errors.length > 1) {
      throw new AggregateError(errors, 'direct assistant reveal reset failed');
    }
  }

  function domIsConsistent(source: string): boolean {
    const root = options.getRoot();
    if (
      !root ||
      !directHost ||
      !directBaseText ||
      !directHost.isConnected ||
      !root.contains(directHost) ||
      directBaseText.parentNode !== directHost ||
      directBaseText.data !== directBaseData ||
      directSource !== source ||
      directHost.childNodes.length !== directTextNodes.length + 1
    ) return false;
    if (directHost.firstChild !== directBaseText) return false;
    for (let index = 0; index < directTextNodes.length; index++) {
      if (directHost.childNodes.item(index + 1) !== directTextNodes[index]) return false;
    }
    return true;
  }

  function adoptHost(
    source: string,
    checkpoint: StreamingAssistantParserCheckpoint,
  ): boolean {
    if (directHost) {
      if (domIsConsistent(source)) return true;
      const root = options.getRoot();
      const sameHostStillMounted = Boolean(
        root && directHost.isConnected && root.contains(directHost),
      );
      reset();
      if (sameHostStillMounted) return false;
    }
    const host = findDirectHost();
    if (!host) return false;
    if (host.childNodes.length !== 1 || !(host.firstChild instanceof Text)) return false;
    // parseIncompleteMarkdown may synthesize punctuation after the source's
    // live text. Most closers become an outer token boundary, where appending
    // inside the marked leaf is correct. Some extensions keep the synthetic
    // byte in the same text token (an unfinished description detail adds a
    // trailing colon). Appending after that byte visibly transposes the next
    // character. The marked host is safe only when its text still ends at the
    // canonical literal tail.
    const baseData = host.firstChild.data;
    const baseTrailingWhitespaceLength = trailingAsciiSpaceCount(baseData);
    const baseVisibleEnd = baseData.length - baseTrailingWhitespaceLength;
    if (
      baseTrailingWhitespaceLength > checkpoint.trailingAsciiSpaces ||
      !rangeEndsWith(
        baseData,
        baseVisibleEnd,
        checkpoint.tailSource,
        checkpoint.tailStart,
        checkpoint.tailEnd,
      )
    ) return false;
    directHost = host;
    directBaseText = host.firstChild;
    directBaseData = directBaseText.data;
    directSource = source;
    pendingWhitespaceLength = Math.max(
      0,
      checkpoint.trailingAsciiSpaces - baseTrailingWhitespaceLength,
    );
    return true;
  }

  function appendVisibleText(delta: string): void {
    if (!directHost) throw new Error('direct assistant reveal lost its text host');
    let offset = 0;
    while (offset < delta.length) {
      if (activeDirectText) {
        const remainingLength = delta.length - offset;
        const room = DIRECT_TEXT_NODE_LIMIT - activeDirectText.length;
        if (room >= remainingLength) {
          activeDirectText.appendData(delta.slice(offset));
          return;
        }
        const end = textRunSafeChunkEnd(
          delta,
          offset,
          activeDirectText.data,
          DIRECT_TEXT_NODE_LIMIT,
        );
        if (end === offset) {
          activeDirectText = null;
          continue;
        }
        const part = delta.slice(offset, end);
        activeDirectText.appendData(part);
        offset += part.length;
        continue;
      }
      if (delta.length - offset <= DIRECT_TEXT_NODE_LIMIT) {
        activeDirectText = document.createTextNode(delta.slice(offset));
        directTextNodes.push(activeDirectText);
        directHost.append(activeDirectText);
        return;
      }
      const end = textRunSafeChunkEnd(
        delta,
        offset,
        '',
        DIRECT_TEXT_NODE_LIMIT,
      );
      const part = delta.slice(offset, end);
      activeDirectText = document.createTextNode(part);
      directTextNodes.push(activeDirectText);
      directHost.append(activeDirectText);
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

  return {
    canAppendLiteral(source, checkpoint, nextSource, delta) {
      return options.canAppendSource(source, nextSource, delta) &&
        adoptHost(source, checkpoint);
    },
    appendLiteral(nextSource, delta) {
      if (!domIsConsistent(directSource)) {
        throw new Error('direct assistant reveal text host changed after preflight');
      }
      appendSource(delta);
      directSource = nextSource;
    },
    restoreLiteral(parserSource, checkpoint, source, directDeltas) {
      let expectedLength = parserSource.length;
      for (const delta of directDeltas) expectedLength += delta.length;
      if (expectedLength !== source.length || !adoptHost(parserSource, checkpoint)) {
        reset();
        return false;
      }
      // The router stores only units that every mounted representation already
      // admitted under the same render context. Replaying those exact units is
      // the proof. Reconstructing one suffix from `source` would flatten the
      // canonical cons rope this path exists to preserve.
      for (const delta of directDeltas) appendSource(delta);
      directSource = source;
      return true;
    },
    reset,
  };
}

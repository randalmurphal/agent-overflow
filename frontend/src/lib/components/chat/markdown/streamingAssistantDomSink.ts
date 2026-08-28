import type { PathRef } from '../../../types/models';
import type { StreamingAssistantRevealSink } from '../../../stores/streamingAssistantReveal';

const DIRECT_TEXT_NODE_LIMIT = 256;

export function sourceCompletesAllowlistedPath(
  pathRefs: readonly PathRef[],
  source: string,
  nextSource: string,
): boolean {
  for (const { path } of pathRefs) {
    let offset = Math.max(0, source.length - path.length + 1);
    while ((offset = nextSource.indexOf(path, offset)) !== -1) {
      if (offset + path.length > source.length) return true;
      offset += 1;
    }
  }
  return false;
}

/**
 * Owns the DOM bytes appended between authoritative markdown renders.
 * The marker host contains one Svelte-owned Text node. Direct bytes live in
 * separate bounded Text nodes after it, so reset can remove every byte it
 * owns without rewriting Svelte's node or disturbing a selection in it.
 */
export function createStreamingAssistantDomSink(options: {
  getRoot(): HTMLElement | undefined;
  canAppendSource(source: string, nextSource: string): boolean;
}): StreamingAssistantRevealSink {
  type StreamingTextHost = HTMLSpanElement;
  let directHost: StreamingTextHost | null = null;
  let directBaseText: Text | null = null;
  let directBaseData = '';
  let directSource = '';
  let directTextNodes: Text[] = [];
  let activeDirectText: Text | null = null;
  let pendingWhitespace = '';

  function findDirectHost(): StreamingTextHost | null {
    const root = options.getRoot();
    if (!root) return null;
    const hosts = root.querySelectorAll<StreamingTextHost>(
      '.md-volatile [data-streamdown-direct-append-safe]',
    );
    return hosts.item(hosts.length - 1) || null;
  }

  function reset(): void {
    for (const node of directTextNodes) node.remove();
    directHost = null;
    directBaseText = null;
    directBaseData = '';
    directSource = '';
    directTextNodes = [];
    activeDirectText = null;
    pendingWhitespace = '';
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

  function adoptHost(source: string): boolean {
    const host = findDirectHost();
    if (!host) return false;
    if (host === directHost) return domIsConsistent(source);

    reset();
    if (host.childNodes.length !== 1 || !(host.firstChild instanceof Text)) return false;
    directHost = host;
    directBaseText = host.firstChild;
    directBaseData = directBaseText.data;
    directSource = source;
    pendingWhitespace = / +$/.exec(source)?.[0] ?? '';
    return true;
  }

  function appendVisibleText(delta: string): void {
    if (!directHost) throw new Error('direct assistant reveal lost its text host');
    let offset = 0;
    while (offset < delta.length) {
      const room = activeDirectText
        ? DIRECT_TEXT_NODE_LIMIT - activeDirectText.length
        : 0;
      if (activeDirectText && room > 0) {
        const part = delta.slice(offset, offset + room);
        activeDirectText.appendData(part);
        offset += part.length;
        continue;
      }
      const part = delta.slice(offset, offset + DIRECT_TEXT_NODE_LIMIT);
      activeDirectText = document.createTextNode(part);
      directTextNodes.push(activeDirectText);
      directHost.append(activeDirectText);
      offset += part.length;
    }
  }

  function appendSource(delta: string): void {
    const sourceDelta = pendingWhitespace + delta;
    const trailing = / +$/.exec(sourceDelta)?.[0] ?? '';
    const visibleLength = sourceDelta.length - trailing.length;
    pendingWhitespace = trailing;
    if (visibleLength > 0) appendVisibleText(sourceDelta.slice(0, visibleLength));
  }

  return {
    canAppendLiteral(source, nextSource) {
      return options.canAppendSource(source, nextSource) && adoptHost(source);
    },
    appendLiteral(nextSource, delta) {
      if (!domIsConsistent(directSource)) {
        throw new Error('direct assistant reveal text host changed after preflight');
      }
      appendSource(delta);
      directSource = nextSource;
    },
    reset,
  };
}

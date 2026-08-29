type SelectionEndpoint =
  | { kind: 'inside'; offset: number }
  | { kind: 'outside'; node: Node; offset: number };

export interface StreamingAssistantSelectionSnapshot {
  root: HTMLElement;
  anchor: SelectionEndpoint;
  focus: SelectionEndpoint;
  textContent: string;
  /** Defined only when both endpoints are inside this message body. */
  selectedText?: string;
}

function textOffsetAt(root: HTMLElement, node: Node, offset: number): number {
  const prefix = document.createRange();
  prefix.selectNodeContents(root);
  prefix.setEnd(node, offset);
  return prefix.toString().length;
}

export function captureStreamingAssistantSelection(
  root: HTMLElement,
): StreamingAssistantSelectionSnapshot | null {
  const selection = window.getSelection();
  const anchorNode = selection?.anchorNode;
  const focusNode = selection?.focusNode;
  if (!selection || !anchorNode || !focusNode) return null;
  const anchorInside = root.contains(anchorNode);
  const focusInside = root.contains(focusNode);
  if (!anchorInside && !focusInside) return null;
  return {
    root,
    anchor: anchorInside
      ? { kind: 'inside', offset: textOffsetAt(root, anchorNode, selection.anchorOffset) }
      : { kind: 'outside', node: anchorNode, offset: selection.anchorOffset },
    focus: focusInside
      ? { kind: 'inside', offset: textOffsetAt(root, focusNode, selection.focusOffset) }
      : { kind: 'outside', node: focusNode, offset: selection.focusOffset },
    textContent: root.textContent ?? '',
    selectedText: anchorInside && focusInside ? selection.toString() : undefined,
  };
}

function commonPrefixLength(left: string, right: string): number {
  const limit = Math.min(left.length, right.length);
  let length = 0;
  while (length < limit && left.charCodeAt(length) === right.charCodeAt(length)) {
    length++;
  }
  return length;
}

function commonSuffixLength(left: string, right: string): number {
  const limit = Math.min(left.length, right.length);
  let length = 0;
  while (
    length < limit &&
    left.charCodeAt(left.length - length - 1) ===
      right.charCodeAt(right.length - length - 1)
  ) length++;
  return length;
}

function mapCollapsedOffset(
  previousText: string,
  nextText: string,
  offset: number,
): number {
  const prefixLength = commonPrefixLength(previousText, nextText);
  if (offset <= prefixLength) return offset;
  const suffixLength = commonSuffixLength(
    previousText.slice(prefixLength),
    nextText.slice(prefixLength),
  );
  if (offset >= previousText.length - suffixLength) {
    return nextText.length - (previousText.length - offset);
  }
  return Math.min(offset, nextText.length);
}

function locateSelectedText(
  snapshot: StreamingAssistantSelectionSnapshot,
  currentText: string,
): number | null {
  if (snapshot.anchor.kind !== 'inside' || snapshot.focus.kind !== 'inside') {
    return null;
  }
  const selectionStart = Math.min(snapshot.anchor.offset, snapshot.focus.offset);
  const selectionEnd = Math.max(snapshot.anchor.offset, snapshot.focus.offset);
  const selectedText = snapshot.selectedText;
  if (selectedText === undefined) return null;
  if (selectedText === '') {
    return mapCollapsedOffset(snapshot.textContent, currentText, selectionStart);
  }

  const contextBefore = snapshot.textContent.slice(
    Math.max(0, selectionStart - 64),
    selectionStart,
  );
  const contextAfter = snapshot.textContent.slice(selectionEnd, selectionEnd + 64);
  let bestOffset = -1;
  let bestContext = -1;
  let bestDistance = Number.POSITIVE_INFINITY;
  for (
    let offset = currentText.indexOf(selectedText);
    offset !== -1;
    offset = currentText.indexOf(selectedText, offset + 1)
  ) {
    const before = currentText.slice(Math.max(0, offset - 64), offset);
    const after = currentText.slice(
      offset + selectedText.length,
      offset + selectedText.length + 64,
    );
    const context = commonSuffixLength(contextBefore, before) +
      commonPrefixLength(contextAfter, after);
    const distance = Math.abs(offset - selectionStart);
    if (
      context > bestContext ||
      (context === bestContext && distance < bestDistance)
    ) {
      bestOffset = offset;
      bestContext = context;
      bestDistance = distance;
    }
  }
  return bestOffset === -1 ? null : bestOffset;
}

function boundaryAtTextOffset(
  root: HTMLElement,
  requestedOffset: number,
): { node: Node; offset: number } {
  let remaining = Math.max(0, requestedOffset);
  const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT);
  let lastText: Text | null = null;
  for (let node = walker.nextNode(); node; node = walker.nextNode()) {
    if (!(node instanceof Text)) continue;
    lastText = node;
    if (remaining <= node.length) return { node, offset: remaining };
    remaining -= node.length;
  }
  if (lastText) return { node: lastText, offset: lastText.length };
  return { node: root, offset: 0 };
}

export function restoreStreamingAssistantSelection(
  snapshot: StreamingAssistantSelectionSnapshot,
): void {
  const selection = window.getSelection();
  const anchorNode = selection?.anchorNode;
  const focusNode = selection?.focusNode;
  if (!selection) return;

  const endpointMatches = (
    endpoint: SelectionEndpoint,
    node: Node | null | undefined,
    offset: number,
  ): boolean => {
    if (!node) return false;
    if (endpoint.kind === 'outside') {
      return node === endpoint.node && offset === endpoint.offset;
    }
    return snapshot.root.contains(node) &&
      textOffsetAt(snapshot.root, node, offset) === endpoint.offset;
  };
  if (
    endpointMatches(snapshot.anchor, anchorNode, selection.anchorOffset) &&
    endpointMatches(snapshot.focus, focusNode, selection.focusOffset) &&
    (snapshot.selectedText === undefined || selection.toString() === snapshot.selectedText)
  ) return;
  const currentText = snapshot.root.textContent ?? '';
  let mappedSelectionStart: number | null = null;
  if (snapshot.anchor.kind === 'inside' && snapshot.focus.kind === 'inside') {
    mappedSelectionStart = locateSelectedText(snapshot, currentText);
    if (mappedSelectionStart === null) return;
  }
  const originalSelectionStart = snapshot.anchor.kind === 'inside' &&
    snapshot.focus.kind === 'inside'
    ? Math.min(snapshot.anchor.offset, snapshot.focus.offset)
    : 0;
  const restoreEndpoint = (
    endpoint: SelectionEndpoint,
  ): { node: Node; offset: number } | null => {
    if (endpoint.kind === 'outside') {
      if (!endpoint.node.isConnected) return null;
      const maxOffset = endpoint.node instanceof CharacterData
        ? endpoint.node.length
        : endpoint.node.childNodes.length;
      return endpoint.offset <= maxOffset ? endpoint : null;
    }
    const offset = mappedSelectionStart === null
      ? mapCollapsedOffset(snapshot.textContent, currentText, endpoint.offset)
      : mappedSelectionStart + endpoint.offset - originalSelectionStart;
    return boundaryAtTextOffset(snapshot.root, offset);
  };
  const anchor = restoreEndpoint(snapshot.anchor);
  const focus = restoreEndpoint(snapshot.focus);
  if (!anchor || !focus) return;
  selection.setBaseAndExtent(
    anchor.node,
    anchor.offset,
    focus.node,
    focus.offset,
  );
}

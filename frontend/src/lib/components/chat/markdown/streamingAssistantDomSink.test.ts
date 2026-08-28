import { describe, expect, it } from 'vitest';
import {
  createStreamingAssistantDomSink,
  sourceCompletesAllowlistedPath,
} from './streamingAssistantDomSink';

function makeRoot(text = 'seed'): {
  root: HTMLElement;
  host: HTMLSpanElement;
  base: Text;
} {
  const root = document.createElement('div');
  const volatile = document.createElement('div');
  volatile.className = 'md-volatile';
  const host = document.createElement('span');
  host.dataset.streamdownDirectAppendSafe = '';
  const base = document.createTextNode(text);
  host.append(base);
  volatile.append(host);
  root.append(volatile);
  document.body.append(root);
  return { root, host, base };
}

describe('streaming assistant DOM sink', () => {
  it('keeps Svelte text untouched and removes only direct nodes on reset', () => {
    const { root, host, base } = makeRoot();
    const sink = createStreamingAssistantDomSink({
      getRoot: () => root,
      canAppendSource: () => true,
    });

    expect(sink.canAppendLiteral('seed ', 'seed next ', 'next ')).toBe(true);
    sink.appendLiteral('seed next ', 'next ');
    expect(host.textContent).toBe('seed next');
    expect(host.firstChild).toBe(base);
    expect(base.data).toBe('seed');
    expect(host.childNodes).toHaveLength(2);

    sink.reset();
    expect(host.textContent).toBe('seed');
    expect(host.childNodes).toHaveLength(1);
    root.remove();
  });

  it('rejects a host changed between direct appends', () => {
    const { root, base } = makeRoot();
    const sink = createStreamingAssistantDomSink({
      getRoot: () => root,
      canAppendSource: () => true,
    });

    expect(sink.canAppendLiteral('seed ', 'seed next ', 'next ')).toBe(true);
    sink.appendLiteral('seed next ', 'next ');
    base.data = 'rewritten ';
    expect(sink.canAppendLiteral(
      'seed next ',
      'seed next word ',
      'word ',
    )).toBe(false);
    sink.reset();
    root.remove();
  });

  it('adopts a remounted authoritative host at the current source', () => {
    const first = makeRoot();
    let root = first.root;
    const sink = createStreamingAssistantDomSink({
      getRoot: () => root,
      canAppendSource: () => true,
    });
    expect(sink.canAppendLiteral('seed ', 'seed next ', 'next ')).toBe(true);
    sink.appendLiteral('seed next ', 'next ');

    first.root.remove();
    const second = makeRoot('seed next');
    root = second.root;
    expect(sink.canAppendLiteral(
      'seed next ',
      'seed next again ',
      'again ',
    )).toBe(true);
    sink.appendLiteral('seed next again ', 'again ');
    expect(second.host.textContent).toBe('seed next again');
    sink.reset();
    second.root.remove();
  });
});

describe('sourceCompletesAllowlistedPath', () => {
  it('detects a path completed across a direct delta boundary', () => {
    expect(sourceCompletesAllowlistedPath(
      [{ path: 'README' }],
      'Open READ',
      'Open README ',
    )).toBe(true);
  });

  it('ignores paths already complete before the delta', () => {
    expect(sourceCompletesAllowlistedPath(
      [{ path: 'README' }],
      'Open README and ',
      'Open README and continue ',
    )).toBe(false);
  });
});

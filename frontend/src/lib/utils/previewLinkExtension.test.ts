import { describe, expect, it } from 'vitest';
import { Lexer } from '../markdown';
import { previewOfToken } from '../markdown/render/previewLink';
import type { PreviewLinkRender } from '../markdown/render/previewLink';
import type { PreviewLinkTarget } from '../stores/devServers.svelte';
import { buildPreviewLinkExtension, parsePreviewTarget } from './previewLinkExtension';

// The extension is driven through a real Lexer rather than by calling its
// tokenizer directly, because half of what it must get right is WHERE marked
// invokes it: this extension has no `start`, on the claim that the gfm inline
// `text` rule already stops at `[`, `<` and `protocol://`. A direct call
// would assert the tokenizer and prove nothing about that claim.

const OPEN_PORTS = new Set([5173]);

function target(overrides: Partial<PreviewLinkTarget> = {}): PreviewLinkTarget {
  return {
    threadId: 'thread-1',
    backend: 'backend-a',
    machine: 'laptop',
    canAllow: false,
    key: 'k',
    resolve: (port) => (OPEN_PORTS.has(port) ? { kind: 'open' } : { kind: 'not-shared' }),
    ...overrides,
  };
}

function lex(text: string, extension = buildPreviewLinkExtension(target())) {
  if (!extension) throw new Error('extension must not be undefined');
  const lexer = new Lexer({
    gfm: true,
    extensions: {
      block: [],
      inline: [extension.tokenizer],
      childTokens: {},
      renderers: {},
      startBlock: [],
      startInline: [],
    },
  } as never);
  return lexer.lex(text);
}

interface FoundLink {
  raw: string;
  href: string;
  title: string | null;
  text: string;
  preview: PreviewLinkRender | undefined;
}

function findLinks(tokens: unknown[]): FoundLink[] {
  const out: FoundLink[] = [];
  const walk = (nodes: unknown[]) => {
    for (const node of nodes) {
      if (!node || typeof node !== 'object') continue;
      const t = node as {
        type?: string;
        raw?: string;
        href?: string;
        title?: string | null;
        text?: string;
        tokens?: unknown[];
      };
      if (t.type === 'link' && typeof t.raw === 'string' && typeof t.href === 'string') {
        out.push({
          raw: t.raw,
          href: t.href,
          title: t.title ?? null,
          text: t.text ?? '',
          preview: previewOfToken(node as never),
        });
      }
      if (Array.isArray(t.tokens)) walk(t.tokens);
    }
  };
  walk(tokens);
  return out;
}

function onlyLink(text: string, extension?: ReturnType<typeof buildPreviewLinkExtension>) {
  const links = findLinks(lex(text, extension) as unknown[]);
  expect(links).toHaveLength(1);
  return links[0];
}

describe('what the preview rewrite claims', () => {
  it('answers nothing without a target, so the surface parses as it did', () => {
    expect(buildPreviewLinkExtension(null)).toBeUndefined();
    expect(buildPreviewLinkExtension(undefined)).toBeUndefined();
  });

  it.each([
    ['a markdown link', 'see [the app](http://localhost:5173/x) now'],
    ['a bare autolink', 'see http://localhost:5173/x now'],
    ['a pointy autolink', 'see <http://localhost:5173/x> now'],
  ])('claims %s', (_name, source) => {
    const link = onlyLink(source);
    expect(link.preview?.state).toBe('open');
    expect(link.preview?.port).toBe(5173);
    expect(link.preview?.path).toBe('/x');
  });

  it('keeps the href the agent wrote, and names where the click really goes', () => {
    const link = onlyLink('[app](http://localhost:5173/x?a=1#top)');
    expect(link.href).toBe('http://localhost:5173/x?a=1#top');
    expect(link.preview?.path).toBe('/x?a=1#top');
    expect(link.text).toBe('app');
  });

  it.each([
    ['localhost', 'http://localhost:5173/'],
    ['a loopback quad', 'http://127.0.0.1:5173/'],
    ['the IPv4 wildcard an agent prints for its own port', 'http://0.0.0.0:5173/'],
    ['the IPv6 loopback', 'http://[::1]:5173/'],
    ['the IPv6 wildcard', 'http://[::]:5173/'],
    ['https on loopback', 'https://localhost:5173/'],
  ])('claims %s', (_name, href) => {
    expect(onlyLink(`[app](${href})`).preview?.port).toBe(5173);
  });

  it.each([
    ['an ordinary site', 'https://example.test/x'],
    ['a name that merely starts with the loopback quad', 'http://127.example.test/x'],
    ['a host with no explicit port', 'http://localhost/x'],
    ['a non-http scheme', 'ws://localhost:5173/x'],
    ['a port out of range', 'http://localhost:99999/x'],
  ])('leaves %s alone', (_name, href) => {
    const link = onlyLink(`[app](${href})`);
    expect(link.preview).toBeUndefined();
  });

  it('leaves an image alone: a screenshot served from a port is not a preview', () => {
    const links = findLinks(lex('![shot](http://localhost:5173/a.png)') as unknown[]);
    expect(links).toHaveLength(0);
  });

  it('does not claim a bare URL inside a link label', () => {
    const links = findLinks(lex('[http://localhost:5173/x](https://example.test/)') as unknown[]);
    expect(links).toHaveLength(1);
    expect(links[0].preview).toBeUndefined();
  });
});

describe('the three answers a rewritten link renders', () => {
  it('is open when the port is shared, and says which machine', () => {
    const link = onlyLink('[app](http://localhost:5173/)');
    expect(link.preview?.state).toBe('open');
    expect(link.title).toBe('Opens on laptop.');
  });

  it('is not shared when the machine has an address but not that port', () => {
    const link = onlyLink('[app](http://localhost:4321/)');
    expect(link.preview?.state).toBe('not-shared');
    expect(link.title).toBe('Port 4321 is not shared from laptop.');
  });

  it('is no-address when the machine has nowhere to serve it from', () => {
    const extension = buildPreviewLinkExtension(
      target({ resolve: () => ({ kind: 'no-address' }) }),
    );
    const link = onlyLink('[app](http://localhost:5173/)', extension);
    expect(link.preview?.state).toBe('no-address');
    expect(link.title).toBe('laptop has no address this page can reach it on.');
  });

  it('carries the thread and machine a click has to be routed by', () => {
    const link = onlyLink('[app](http://localhost:5173/)');
    expect(link.preview?.threadId).toBe('thread-1');
    expect(link.preview?.backend).toBe('backend-a');
    expect(link.preview?.canAllow).toBe(false);
  });
});

describe('parsePreviewTarget', () => {
  it('defaults an empty path to the root', () => {
    expect(parsePreviewTarget('http://localhost:5173')).toEqual({ port: 5173, path: '/' });
  });

  it.each([[null], [undefined], [42], ['']])('refuses %s', (href) => {
    expect(parsePreviewTarget(href)).toBeNull();
  });
});

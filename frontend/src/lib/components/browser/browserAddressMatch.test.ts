import { describe, expect, it } from 'vitest';
import { addressSuggestions, SUGGESTION_LIMITS, threadDevServers, type AddressSuggestionSources } from './browserAddressMatch';

function sources(extra: Partial<AddressSuggestionSources> = {}): AddressSuggestionSources {
  return {
    tabs: [
      { id: 'a', url: 'https://example.test/active', title: 'Active page' },
      { id: 'b', url: 'http://localhost:5173/settings', title: 'Settings' },
      { id: 'c', url: 'https://docs.example.test/guide', title: 'Guide to localhost' },
    ],
    activePageId: 'a',
    devServers: [
      { url: 'http://localhost:5173/', title: 'vite on port 5173' },
      { url: 'http://localhost:8080/', title: 'node on port 8080' },
    ],
    recent: [
      { url: 'https://www.github.test/org/repo', title: 'Repo' },
      { url: 'http://localhost:8080/', title: 'API' },
      { url: 'https://example.test/active', title: 'Active page' },
    ],
    ...extra,
  };
}

const addresses = (typed: string, from = sources()) => addressSuggestions(typed, from).map((s) => `${s.kind} ${s.address}`);

describe('addressSuggestions', () => {
  it('offers nothing for blank text', () => {
    expect(addressSuggestions('', sources())).toEqual([]);
    expect(addressSuggestions('   ', sources())).toEqual([]);
  });

  it('lists tabs, then dev servers, then recent addresses, each address once', () => {
    expect(addresses('localhost')).toEqual([
      'tab http://localhost:5173/settings',
      // Matched by its title only, so after the address match.
      'tab https://docs.example.test/guide',
      'dev-server http://localhost:5173/',
      'dev-server http://localhost:8080/',
    ]);
    // The dev server already offers :8080, so the recent entry is not repeated.
    expect(addresses('8080')).toEqual(['dev-server http://localhost:8080/']);
  });

  it('never offers the field\'s own tab, under any source', () => {
    expect(addresses('example.test/active')).toEqual([]);
    expect(addresses('active')).toEqual([]);
  });

  it('carries the tab to switch to', () => {
    const [tab] = addressSuggestions('settings', sources());
    expect(tab).toMatchObject({ kind: 'tab', pageId: 'b', title: 'Settings' });
  });

  it('ranks an address that starts with the text first, ignoring scheme and www', () => {
    const from = sources({
      tabs: [],
      devServers: [],
      recent: [
        { url: 'https://other.test/github', title: '' },
        { url: 'https://www.github.test/org/repo', title: '' },
      ],
    });
    expect(addresses('github', from)).toEqual(['recent https://www.github.test/org/repo', 'recent https://other.test/github']);
    // "h" is not the start of every address because each has a scheme.
    expect(addresses('h', from)).toEqual(['recent https://other.test/github', 'recent https://www.github.test/org/repo']);
    expect(addresses('https://other', from)).toEqual(['recent https://other.test/github']);
  });

  it('matches every word against the address and title together', () => {
    expect(addresses('guide localhost')).toEqual(['tab https://docs.example.test/guide']);
    expect(addresses('guide nowhere')).toEqual([]);
  });

  it('skips tabs with no page to switch to', () => {
    const from = sources({
      tabs: [
        { id: 'a', url: 'https://example.test/active', title: '' },
        { id: 'n', url: 'about:blank', title: 'New tab' },
        { id: 'p', url: 'https://example.test/pending', title: '', creating: true },
        { id: 'f', url: 'https://example.test/failed', title: '', error: 'no' },
      ],
      devServers: [],
      recent: [],
    });
    expect(addresses('example', from)).toEqual([]);
    expect(addresses('new tab', from)).toEqual([]);
  });

  it('bounds each source and the list', () => {
    const many = (prefix: string) => Array.from({ length: 10 }, (_, i) => `https://${prefix}${i}.test/`);
    const from: AddressSuggestionSources = {
      tabs: many('tab').map((url, i) => ({ id: `t${i}`, url, title: '' })),
      activePageId: '',
      devServers: many('dev').map((url) => ({ url, title: '' })),
      recent: many('recent').map((url) => ({ url })),
    };
    const kinds = addressSuggestions('test', from).map((s) => s.kind);
    expect(kinds.length).toBe(SUGGESTION_LIMITS.total);
    expect(kinds.filter((k) => k === 'tab').length).toBe(SUGGESTION_LIMITS.tab);
    expect(kinds.filter((k) => k === 'dev-server').length).toBe(SUGGESTION_LIMITS['dev-server']);
    expect(kinds.filter((k) => k === 'recent').length).toBe(SUGGESTION_LIMITS.total - SUGGESTION_LIMITS.tab - SUGGESTION_LIMITS['dev-server']);
  });

  it('tells destinations apart by everything but scheme and host case and an empty path', () => {
    const from = sources({
      tabs: [
        { id: 'a', url: 'file:///tmp/Report.html', title: '' },
        { id: 'b', url: 'http://example.test/Path?Q=1', title: '' },
      ],
      devServers: [],
      recent: [
        // A different file than the field's own tab: paths are case-sensitive.
        { url: 'file:///tmp/report.html' },
        // A different scheme, and a different query, than tab b.
        { url: 'https://example.test/Path?Q=1' },
        { url: 'http://example.test/Path?q=1' },
        // Tab b again, spelled with an upper-case scheme and host.
        { url: 'HTTP://EXAMPLE.TEST/Path?Q=1' },
        // One destination two ways: the empty path is the root.
        { url: 'https://Example.test' },
        { url: 'https://example.test/' },
        // Text the parser refuses compares exactly.
        { url: 'not an address' },
        { url: 'Not an address' },
      ],
    });
    expect(addresses('report', from)).toEqual(['recent file:///tmp/report.html']);
    expect(addresses('path', from)).toEqual([
      'tab http://example.test/Path?Q=1',
      'recent https://example.test/Path?Q=1',
      'recent http://example.test/Path?q=1',
    ]);
    expect(addresses('https://example', from)).toEqual(['recent https://example.test/Path?Q=1', 'recent https://Example.test']);
    expect(addresses('address', from)).toEqual(['recent not an address', 'recent Not an address']);
  });

  it('keys are unique even for two tabs on one address', () => {
    const from = sources({
      tabs: [
        { id: 'x', url: 'https://same.test/', title: '' },
        { id: 'y', url: 'https://same.test/', title: '' },
      ],
    });
    const found = addressSuggestions('same', from);
    expect(found.map((s) => s.key)).toEqual(['tab:x']);
  });
});

describe('threadDevServers', () => {
  it('keeps the thread\'s listening servers as local addresses', () => {
    expect(
      threadDevServers('t1', [
        { port: 5173, threadId: 't1', process: 'vite', listening: true, scheme: 'http' },
        { port: 8443, threadId: 't1', listening: true, scheme: 'https' },
        { port: 3000, threadId: 't2', process: 'next', listening: true },
        { port: 4000, threadId: 't1', process: 'node', listening: false },
        { port: 9000, listening: true },
      ]),
    ).toEqual([
      { url: 'http://localhost:5173/', title: 'vite on port 5173' },
      { url: 'https://localhost:8443/', title: 'Port 8443' },
    ]);
  });
});

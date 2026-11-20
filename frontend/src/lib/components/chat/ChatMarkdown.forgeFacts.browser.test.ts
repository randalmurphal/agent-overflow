import { render, waitFor } from '@testing-library/svelte';
import { tick } from 'svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import '../../../app.css';
import ChatMarkdown from './ChatMarkdown.svelte';
import { FORGE_ATTACHMENT_SOURCE_CONTEXT } from './markdown/forgeAttachmentContext';
import { EMPTY_PATH_REFS } from '../../utils/pathLinkify';
import { Lexer } from '../../markdown';
import {
  FACT_RETRY_MS,
  __forgeFactsHeldForTest,
  __resetForgeFactsForTest,
  __setResolveForgeFactsForTest,
  forgeFactReader,
  observePRFacts,
  type ResolveForgeFactsCall,
} from '../../stores/forgeFacts.svelte';
import type { ForgeAttachmentSource } from '../../utils/forgeAttachments';
import type { ForgeFact } from '../../utils/forgeFacts';
import type { PRDetail } from '../../types/models';
import { prKey } from '../../utils/prReference';
import { loadForgeEmoji } from '../../utils/forgeEmoji.svelte';

// Review bodies under the review pane's facts, as ReviewPane provides them:
// a late fact rebuilds only the blocks that read it, lexes nothing else,
// and moves nothing on the page.

const GITHUB: ForgeAttachmentSource = {
  pr: { forge: 'github', host: 'github.com', namespace: 'acme', repo: 'widget', number: 4 },
  backend: 'home',
  webBase: 'https://github.com/acme/widget/pull/4',
};
const GITLAB: ForgeAttachmentSource = {
  pr: { forge: 'gitlab', host: 'gitlab.com', namespace: 'group', repo: 'widget', number: 3 },
  backend: 'home',
  webBase: 'https://gitlab.com/group/widget/-/merge_requests/3',
};
const GL = 'https://gitlab.com/group/widget';

let calls: { keys: readonly string[]; answer: (facts: ForgeFact[]) => void }[] = [];

beforeEach(async () => {
  // The emoji table arriving changes the extension list, which lexes and
  // sanitizes every block again; a count from here on is the facts' alone.
  await loadForgeEmoji();
  __resetForgeFactsForTest();
  calls = [];
  const call: ResolveForgeFactsCall = (_b, _w, _p, _r, keys) => new Promise((resolve) => calls.push({ keys: [...keys], answer: (facts) => resolve({ facts }) }));
  __setResolveForgeFactsForTest(call);
  document.body.innerHTML = '';
});

afterEach(() => {
  __resetForgeFactsForTest();
  document.body.innerHTML = '';
});

/** The PR's snapshot with these bodies, as the PR store applies it. */
function applySnapshot(source: ForgeAttachmentSource, ...bodies: string[]): string {
  const key = prKey(source.pr);
  observePRFacts(key, source.pr, {
    detail: { number: source.pr.number, body: bodies[0], url: source.webBase, latestReviews: [] } as unknown as PRDetail,
    threads: [{ id: 't', comments: bodies.slice(1).map((body, index) => ({ id: `c${index}`, body })) } as never],
  });
  return key;
}

async function answer(facts: (keys: readonly string[]) => ForgeFact[]): Promise<void> {
  const out = calls.splice(0);
  for (const call of out) call.answer(facts(call.keys));
  for (let i = 0; i < 6; i++) await tick();
  await new Promise((resolve) => requestAnimationFrame(() => resolve(null)));
}

function mount(source: ForgeAttachmentSource, key: string, body: string, host: HTMLElement): Element {
  const { container } = render(ChatMarkdown, {
    target: host,
    props: { source: body, pathRefs: EMPTY_PATH_REFS, embeddedHtml: true, class: 'review-prose' },
    context: new Map([[FORGE_ATTACHMENT_SOURCE_CONTEXT, () => ({ ...source, facts: forgeFactReader(key) })]]),
  });
  return container.querySelector('.markdown-body')!;
}

function hostOf(width = 640): HTMLElement {
  const host = document.createElement('div');
  host.style.width = `${width}px`;
  document.body.appendChild(host);
  return host;
}

/** Every inline lex from now on, by the source it lexed. */
function recordLexes(): { sources: string[]; restore: () => void } {
  const sources: string[] = [];
  const original = Lexer.prototype.inlineTokens;
  const spy = vi.spyOn(Lexer.prototype, 'inlineTokens').mockImplementation(function (this: Lexer, src: string, tokens?: Parameters<Lexer['inlineTokens']>[1]) {
    sources.push(src);
    return original.call(this, src, tokens);
  });
  return { sources, restore: () => spy.mockRestore() };
}

/** The renderer's deferred static builds done, so a lex after this is the fact's. */
async function settled(): Promise<void> {
  for (let i = 0; i < 3; i++) await new Promise((resolve) => setTimeout(resolve, 50));
}

const found = (key: string, extra: Partial<ForgeFact> = {}): ForgeFact => ({ key, status: 'found', ...extra });
const LABELS = 'labels:group/widget';
/** The project's whole label list, every label in one color. */
const labelList = (titles: string[]): ForgeFact =>
  found(LABELS, { complete: true, labels: titles.map((title) => ({ title, color: '#d9534f', textColor: '#ffffff' })) });

describe('a body no snapshot carries (a draft, an editor preview)', () => {
  it('gets the facts it reads: asked once when it mounts, a title when found, text when the forge has none', async () => {
    const key = applySnapshot(GITHUB, 'The description names nothing.');
    expect(calls).toHaveLength(0);
    const root = mount(GITHUB, key, 'A draft naming #41 and #42.', hostOf());
    await waitFor(() => expect(calls.map((call) => [...call.keys].sort())).toEqual([['ref:acme/widget#41', 'ref:acme/widget#42']]));
    await waitFor(() => expect(root.querySelectorAll('a')).toHaveLength(2));
    await answer((keys) => keys.map((each) => (each.endsWith('#41') ? found(each, { url: 'https://github.com/acme/widget/issues/41', title: 'Forty one' }) : { key: each, status: 'absent' as const })));
    await waitFor(() => expect([...root.querySelectorAll('a')].map((a) => [a.textContent, a.getAttribute('title')])).toEqual([['#41', 'Forty one']]));
    expect(root.textContent).toContain('and #42.');
    await settled();
    expect(calls).toHaveLength(0);
  });
});

describe('a fact arriving for mounted bodies', () => {
  const READS = 'First paragraph.\n\nSee #1 here.\n\nThird paragraph.';
  const READS_NOTHING = 'Nothing to read.\n\nAnother plain block.';

  it('rebuilds only the block that read it, and lexes no other body', async () => {
    const key = applySnapshot(GITHUB, READS, READS_NOTHING, 'Elsewhere #9');
    const reads = mount(GITHUB, key, READS, hostOf());
    const other = mount(GITHUB, key, READS_NOTHING, hostOf());
    await waitFor(() => expect(reads.querySelector('a')?.getAttribute('href')).toBe('https://github.com/acme/widget/issues/1'));
    await settled();
    const readsBlocks = [...reads.querySelectorAll('p')];
    const otherBlocks = [...other.querySelectorAll('p')];
    expect(readsBlocks).toHaveLength(3);
    expect(otherBlocks).toHaveLength(2);

    const lexes = recordLexes();
    try {
      await answer((keys) => keys.map((each) => found(each, { url: `https://github.com/acme/widget/pull/${each.split('#')[1]}`, title: 'A pull request' })));
      await waitFor(() => expect(reads.querySelector('a')?.getAttribute('href')).toBe('https://github.com/acme/widget/pull/1'));
      expect(reads.querySelector('a')?.getAttribute('title')).toBe('A pull request');
      const after = [...reads.querySelectorAll('p')];
      expect(after[0]).toBe(readsBlocks[0]);
      expect(after[1]).not.toBe(readsBlocks[1]);
      expect(after[2]).toBe(readsBlocks[2]);
      expect([...other.querySelectorAll('p')]).toEqual(otherBlocks);
      [...other.querySelectorAll('p')].forEach((block, index) => expect(block).toBe(otherBlocks[index]));
      // One lex: the block that read #1.
      expect(lexes.sources).toEqual(['See #1 here.']);
    } finally {
      lexes.restore();
    }
  });

  it('relexes no block for a fact no mounted block read', async () => {
    const key = applySnapshot(GITHUB, READS, 'Elsewhere #9');
    const reads = mount(GITHUB, key, READS, hostOf());
    await waitFor(() => expect(reads.querySelector('a')).not.toBeNull());
    await settled();
    // #1 settles first; then only #9, which no mounted body names, moves.
    await answer((keys) => keys.map((each) => ({ key: each, status: 'unavailable' as const })));
    const blocks = [...reads.querySelectorAll('p')];
    // An unavailable answer is asked for again a minute after it.
    vi.useFakeTimers({ toFake: ['Date'] });
    vi.setSystemTime(Date.now() + FACT_RETRY_MS);
    applySnapshot(GITHUB, READS, 'Elsewhere #9', 'edited to ask again');
    const lexes = recordLexes();
    try {
      expect(calls.map((call) => call.keys)).toEqual([['ref:acme/widget#1', 'ref:acme/widget#9']]);
      await answer(() => [{ key: 'ref:acme/widget#1', status: 'unavailable' }, found('ref:acme/widget#9', { url: 'https://github.com/acme/widget/issues/9' })]);
      expect(lexes.sources).toEqual([]);
      [...reads.querySelectorAll('p')].forEach((block, index) => expect(block).toBe(blocks[index]));
    } finally {
      lexes.restore();
      vi.useRealTimers();
    }
  });
});

/** Every sanitize from now on: one DOMParser pass per html token. */
function recordParses(): { count: () => number; sources: () => string[]; restore: () => void } {
  const spy = vi.spyOn(DOMParser.prototype, 'parseFromString');
  return {
    count: () => spy.mock.calls.length,
    sources: () => spy.mock.calls.map((call) => String(call[0])),
    restore: () => spy.mockRestore(),
  };
}

describe('text inside HTML under the facts', () => {
  const BLOCKS = [
    '<div>First <b>block</b> names #1 here.</div>',
    '<div>Second block names nothing.</div>',
    '<div>Third block, also plain.</div>',
    '<table><tr><td>Fourth</td><td>plain cell</td></tr></table>',
    '<div>Fifth names @ana.</div>',
    '<div>Sixth plain.</div>',
  ];
  const BODY = BLOCKS.join('\n\n');

  it('reads a fact as a paragraph does, and sanitizes again only the block that read it', async () => {
    const key = applySnapshot(GITHUB, BODY, 'Elsewhere #9');
    const root = mount(GITHUB, key, BODY, hostOf());
    await waitFor(() => expect(root.querySelector('a[href="https://github.com/acme/widget/issues/1"]')).not.toBeNull());
    await settled();
    expect(calls.map((call) => call.keys)).toEqual([['ref:acme/widget#1', 'user:ana', 'ref:acme/widget#9']]);
    const blocks = [...root.querySelectorAll('.md-committed > *')];
    // Pending is the shape: #1 links where an issue would be.
    const pendingRects = blocks.map((block) => block.getBoundingClientRect().toJSON());

    const parses = recordParses();
    try {
      await answer(() => [
        found('ref:acme/widget#1', { url: 'https://github.com/acme/widget/pull/1', title: 'A pull request' }),
        found('user:ana', { url: 'https://github.com/ana' }),
        { key: 'ref:acme/widget#9', status: 'unavailable' },
      ]);
      await waitFor(() => expect(root.querySelector('a[href="https://github.com/acme/widget/pull/1"]')?.getAttribute('title')).toBe('A pull request'));
      // The first and fifth blocks read the facts that changed; the fifth
      // renders as it did (its answer is its shape).
      await settled();
      expect(parses.sources().map((source) => source.trim())).toEqual([BLOCKS[0], BLOCKS[4]]);
      // Facts no block read, three times: nothing is sanitized, the fifth
      // included.
      const version = __forgeFactsHeldForTest(key)!.version;
      for (const number of [20, 21, 22]) {
        applySnapshot(GITHUB, BODY, 'Elsewhere #9', `and #${number}`);
        await answer((keys) => keys.map((each) => found(each, { url: `https://github.com/acme/widget/issues/${number}` })));
        await settled();
      }
      expect(__forgeFactsHeldForTest(key)!.version).toBeGreaterThanOrEqual(version + 3);
      expect(parses.count()).toBe(2);
    } finally {
      parses.restore();
    }
    const after = [...root.querySelectorAll('.md-committed > *')];
    for (const at of [1, 2, 3]) expect(after[at]).toBe(blocks[at]);
    expect(after.map((block) => block.getBoundingClientRect().toJSON())).toEqual(pendingRects);
  });

  it('sanitizes exactly one of N html blocks when the fact one of them read arrives', async () => {
    const body = Array.from({ length: 8 }, (_, at) => (at === 5 ? '<div>Only this one names #5.</div>' : `<div>Block ${at} is plain.</div>`)).join('\n\n');
    const key = applySnapshot(GITHUB, body);
    const root = mount(GITHUB, key, body, hostOf());
    await waitFor(() => expect(root.querySelector('a[href="https://github.com/acme/widget/issues/5"]')).not.toBeNull());
    await settled();
    const parses = recordParses();
    try {
      await answer((keys) => keys.map((each) => ({ key: each, status: 'absent' as const })));
      await waitFor(() => expect(root.querySelector('a')).toBeNull());
      await settled();
      expect(parses.count()).toBe(1);
    } finally {
      parses.restore();
    }
  });

  it('reads a region\'s HTML text under its block\'s facts', async () => {
    const body = '<div>Region text names #4.\n\nAn island names #3.\n\n</div>';
    const key = applySnapshot(GITHUB, body);
    const root = mount(GITHUB, key, body, hostOf());
    await waitFor(() => expect(root.querySelectorAll('a')).toHaveLength(2));
    await settled();
    await answer((keys) => keys.map((each) => found(each, { url: `https://github.com/acme/widget/pull/${each.split('#')[1]}` })));
    await waitFor(() => expect([...root.querySelectorAll('a')].map((a) => a.getAttribute('href')).sort()).toEqual([
      'https://github.com/acme/widget/pull/3',
      'https://github.com/acme/widget/pull/4',
    ]));
  });

  it('draws a GitLab label in HTML text as the chip a paragraph draws, the same box pending and found', async () => {
    const body = '<div>Labels ~bug and ~"two words" in HTML, ~type::bug too.</div>\n\nAfter.';
    const key = applySnapshot(GITLAB, body);
    const host = hostOf(420);
    const root = mount(GITLAB, key, body, host);
    await waitFor(() => expect(root.querySelectorAll('[data-streamdown-label-chip]')).toHaveLength(3));
    await document.fonts.ready;
    const chips = () => [...root.querySelectorAll('[data-streamdown-label-chip]')];
    expect(chips().map((chip) => chip.textContent)).toEqual(['bug', 'two words', 'typebug']);
    expect(chips()[2].className).toBe('md-label-chip md-label-chip-scoped');
    const before = rects(root, host.lastElementChild!);
    const boxes = chips().map((chip) => chip.getBoundingClientRect().toJSON());
    expect(calls.map((call) => call.keys)).toEqual([[LABELS]]);
    await answer(() => [labelList(['bug', 'two words', 'type::bug'])]);
    await waitFor(() => expect(chips()[0].querySelector('.md-label-chip-key')?.getAttribute('style')).toContain('background-color'));
    expect(chips()[0].getAttribute('href')).toBe(`${GL}/-/issues?label_name=bug`);
    expect(rects(root, host.lastElementChild!)).toEqual(before);
    expect(chips().map((chip) => chip.getBoundingClientRect().toJSON())).toEqual(boxes);
  });
});

describe('a mounted body under a found label list', () => {
  /** After `minutes` of frames, the keys each frame's call asked for. */
  async function askedOver(body: string, minutes: number): Promise<(readonly string[])[]> {
    for (let minute = 1; minute <= minutes; minute++) {
      vi.setSystemTime(Date.now() + 60 * 1000);
      applySnapshot(GITLAB, body, `frame ${minute}`);
    }
    return calls.map((call) => call.keys);
  }

  afterEach(() => {
    vi.useRealTimers();
  });

  // The asking follows what the lexes decided: a list every named label is
  // in is never asked again by the clock, one that lacks a label a block
  // names, in a paragraph or in HTML text, ten minutes after its answer.
  it.each([
    ['a paragraph names a label it lacks', 'Labels ~bug and ~docs.\n\nAfter.', true],
    ['HTML text names a label it lacks', 'Labels ~bug.\n\n<div>In HTML ~docs.</div>\n\nAfter.', true],
    ['every label named is in it', 'Labels ~bug and ~docs.\n\n<div>In HTML ~bug.</div>\n\nAfter.', false],
  ] as const)('asks again by the clock when %s: %j', async (_name, body, missing) => {
    vi.useFakeTimers({ toFake: ['Date'] });
    const key = applySnapshot(GITLAB, body);
    const root = mount(GITLAB, key, body, hostOf());
    await waitFor(() => expect(root.querySelectorAll('[data-streamdown-label-chip]').length).toBeGreaterThan(0));
    await answer(() => [labelList(missing ? ['bug'] : ['bug', 'docs'])]);
    await waitFor(() => expect(root.querySelector('[data-streamdown-label-chip] .md-label-chip-key')?.getAttribute('style') ?? '').toContain('background-color'));
    expect(root.querySelectorAll('[data-streamdown-label-chip]')).toHaveLength(missing ? 1 : 3);
    expect(await askedOver(body, 9)).toEqual([]);
    expect(await askedOver(body, 51)).toEqual(missing ? [[LABELS]] : []);
  });
});

describe('a computer without ResolveForgeFacts', () => {
  it('renders later bodies by shape from the first paint, asks nothing and moves nothing', async () => {
    let asked = 0;
    __setResolveForgeFactsForTest(async () => {
      asked++;
      return null;
    });
    // The first PR learns it.
    applySnapshot(GITLAB, 'First ~bug');
    await answer(() => []);
    expect(asked).toBe(1);
    const source: ForgeAttachmentSource = { ...GITLAB, pr: { ...GITLAB.pr, number: 9 } };
    const body = 'Refs #1 and @ana with ~bug and %v1.\n\n<div>HTML ~bug and #2</div>';
    const key = applySnapshot(source, body);
    expect(forgeFactReader(key)!.get(LABELS)).toBeUndefined();
    const host = hostOf(420);
    const root = mount(source, key, body, host);
    await waitFor(() => expect(root.querySelectorAll('a').length).toBeGreaterThan(0));
    await document.fonts.ready;
    // By shape: references link, labels and milestones are text.
    expect(root.querySelectorAll('[data-streamdown-label-chip]')).toHaveLength(0);
    expect(root.textContent).toContain('~bug and %v1');
    const before = rects(root, host.lastElementChild!);
    await answer(() => []);
    await settled();
    expect(asked).toBe(1);
    expect(rects(root, host.lastElementChild!)).toEqual(before);
  });
});

/** The rect of every block, of each reference and of the element after the body. */
function rects(root: Element, after: Element): string[] {
  const of = (element: Element) => {
    const rect = element.getBoundingClientRect();
    return `${element.tagName}:${rect.x},${rect.y},${rect.width},${rect.height}`;
  };
  const blocks = [...root.querySelectorAll('.md-committed > *')];
  return [...blocks.map(of), of(after)];
}

/** The width of a paragraph's inline content, on one line. */
function inlineWidth(paragraph: Element): number {
  const range = document.createRange();
  range.selectNodeContents(paragraph);
  return range.getBoundingClientRect().width;
}

const GEOMETRY = [
  'Refs #1, !2 and @ana with ~bug, ~type::bug and ~"two words" plus %v1, $3 and &4 in a long line that wraps across the width of the pane so that a change in any width shows.',
  '- #5 and @bo in a list\n- ~"priority::high" chip',
  '| a | b |\n| --- | --- |\n| #6 | ~bug |',
  'Tail paragraph.',
].join('\n\n');

async function mountForGeometry(): Promise<{ root: Element; after: Element; key: string }> {
  const key = applySnapshot(GITLAB, GEOMETRY);
  const host = hostOf(420);
  const root = mount(GITLAB, key, GEOMETRY, host);
  const after = document.createElement('div');
  after.textContent = 'after the body';
  host.appendChild(after);
  await waitFor(() => expect(root.querySelectorAll('[data-streamdown-label-chip]')).toHaveLength(5));
  await document.fonts.ready;
  return { root, after, key };
}

describe('geometry when facts arrive', () => {
  it('moves nothing from pending to found, references and labels alike', async () => {
    const { root, after } = await mountForGeometry();
    const before = rects(root, after);
    const chips = [...root.querySelectorAll('[data-streamdown-label-chip]')].map((chip) => chip.getBoundingClientRect().toJSON());
    await answer((keys) => keys.map((key) => key === LABELS
      ? labelList(['bug', 'type::bug', 'two words', 'priority::high'])
      : found(key, { url: `${GL}/-/issues/1`, title: 'Title' })));
    await waitFor(() => expect(root.querySelector('[data-streamdown-label-chip] .md-label-chip-key')?.getAttribute('style')).toContain('background-color'));
    expect(rects(root, after)).toEqual(before);
    expect([...root.querySelectorAll('[data-streamdown-label-chip]')].map((chip) => chip.getBoundingClientRect().toJSON())).toEqual(chips);
  });

  it('paints a body in its final shape in its first frame while the label list is held, and moves nothing after', async () => {
    const key = applySnapshot(GITLAB, 'The description has ~bug.');
    await answer(() => [labelList(['bug', 'type::bug'])]);
    const body = 'A real ~bug, a mistyped ~bgu and a scoped ~type::bug in a long line that wraps across the width of the pane so that any change of width shows up.\n\nTail.';
    applySnapshot(GITLAB, 'The description has ~bug.', body);
    expect(calls).toHaveLength(0);
    const host = hostOf(420);
    const root = mount(GITLAB, key, body, host);
    const after = document.createElement('div');
    after.textContent = 'after the body';
    host.appendChild(after);
    await tick();
    const chips = () => [...root.querySelectorAll('[data-streamdown-label-chip]')];
    const first = { chips: chips().map((chip) => [chip.textContent, chip.querySelector('.md-label-chip-key')?.getAttribute('style') ?? '']), text: root.textContent };
    expect(first.chips).toEqual([['bug', expect.stringContaining('background-color')], ['typebug', expect.stringContaining('background-color')]]);
    expect(first.text).toContain('a mistyped ~bgu and');
    await document.fonts.ready;
    const before = rects(root, after);
    await settled();
    await new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve)));
    expect(calls).toHaveLength(0);
    expect(rects(root, after)).toEqual(before);
    expect(chips().map((chip) => [chip.textContent, chip.querySelector('.md-label-chip-key')?.getAttribute('style') ?? ''])).toEqual(first.chips);
  });

  it('moves nothing from pending to absent for references', async () => {
    const source = 'Refs #1, !2, @ana and group/other#7 in a long line that wraps across the width of the pane so that any change of width shows up.\n\nTail.';
    const key = applySnapshot(GITLAB, source);
    const host = hostOf(420);
    const root = mount(GITLAB, key, source, host);
    const after = document.createElement('div');
    after.textContent = 'after the body';
    host.appendChild(after);
    await waitFor(() => expect(root.querySelectorAll('a')).toHaveLength(4));
    await document.fonts.ready;
    const before = rects(root, after);
    await answer((keys) => keys.map((each) => ({ key: each, status: 'absent' as const })));
    await waitFor(() => expect(root.querySelectorAll('a')).toHaveLength(0));
    expect(rects(root, after)).toEqual(before);
  });

  it('draws a number written with a tilde or a dollar as text while asked, and moves nothing when the forge has no such label or snippet', async () => {
    const source = 'It takes ~5 minutes, ~10ms per call and $3 a run, in a long line that wraps across the width of the pane so that any change of width shows up.\n\nTail.';
    const key = applySnapshot(GITLAB, source);
    const host = hostOf(420);
    const root = mount(GITLAB, key, source, host);
    const after = document.createElement('div');
    after.textContent = 'after the body';
    host.appendChild(after);
    await waitFor(() => expect(root.textContent).toContain('~5 minutes, ~10ms per call and $3 a run'));
    expect(forgeFactReader(key)!.get(LABELS)).toBe('pending');
    expect(root.querySelector('[data-streamdown-label-chip], a')).toBeNull();
    await document.fonts.ready;
    const before = rects(root, after);
    await answer((keys) => keys.map((each) => ({ key: each, status: 'absent' as const })));
    expect(forgeFactReader(key)!.get(LABELS)).toMatchObject({ status: 'absent' });
    await new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve)));
    expect(root.querySelector('[data-streamdown-label-chip], a')).toBeNull();
    expect(root.textContent).toContain('~5 minutes, ~10ms per call and $3 a run');
    expect(rects(root, after)).toEqual(before);
  });

  it('gives a linked and an unlinked #12 the same width in review prose', async () => {
    const key = applySnapshot(GITHUB, 'x #12 y');
    const host = hostOf();
    const root = mount(GITHUB, key, 'x #12 y', host);
    await waitFor(() => expect(root.querySelector('a')).not.toBeNull());
    await document.fonts.ready;
    const linked = inlineWidth(root.querySelector('p')!);
    await answer((keys) => keys.map((each) => ({ key: each, status: 'absent' as const })));
    await waitFor(() => expect(root.querySelector('a')).toBeNull());
    expect(inlineWidth(root.querySelector('p')!)).toBe(linked);
  });
});

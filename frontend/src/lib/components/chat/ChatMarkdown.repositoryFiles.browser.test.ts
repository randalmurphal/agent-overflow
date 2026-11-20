import { render, waitFor } from '@testing-library/svelte';
import { flushSync, tick } from 'svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import '../../../app.css';
import ChatMarkdown from './ChatMarkdown.svelte';
import ForgeAttachmentChatHarness from './ForgeAttachmentChatHarness.svelte';
import StreamdownImageHost from './markdown/StreamdownImageHost.svelte';
import { FORGE_ATTACHMENT_SOURCE_CONTEXT } from './markdown/forgeAttachmentContext';
import { markdownFindText } from './markdown/markdownFindText';
import { resetCodeSpanCacheForTest } from './markdown/codeSpanCache';
import { __resetStreamdownCodeHostForTest } from './markdown/StreamdownCodeHost.svelte';
import { EMPTY_PATH_REFS } from '../../utils/pathLinkify';
import { Lexer } from '../../markdown';
import { findTextSegments } from '../../utils/findHighlights';
import { foldFindText } from '../../utils/findMatch';
import {
  __resetForgeFactsForTest,
  __setResolveForgeFactsForTest,
  forgeFactReader,
  forgeFactsCounters,
  noteForgeFactsWorkspace,
  observePRFacts,
  type ResolveForgeFactsCall,
} from '../../stores/forgeFacts.svelte';
import type { ForgeAttachmentSource } from '../../utils/forgeAttachments';
import { imageFactKey, linesFactKey, type ForgeFact } from '../../utils/forgeFacts';
import { __resetImageTiersForTest } from '../../utils/imageTiers';
import { __resetMediaBlobCacheForTest, mediaBlobCacheStats } from '../../utils/mediaBlobCache';
import { buildLocalImageHref } from '../../utils/pathLinkExtension';
import { prKey } from '../../utils/prReference';
import { composeWorkspaceKey } from '../../utils/workspaceKey';
import type { PRDetail } from '../../types/models';
import type { WorkspaceRef } from '../../types/git';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { getToasts } from '../../stores/toast.svelte';
import { mockGitBlobImage, mockLocalImage, resetAttachmentTransferMocks } from '../../../test/mocks/attachmentTransfer';

// In real Chromium under the app stylesheet: a PR body's repository images
// and permalink snippets as the review pane mounts them, under its facts.
// Each box is the painted one from the first frame its size is knowable,
// a fact or the bytes landing rebuild only the block that read them, and
// nothing is fetched for an image that never mounts.

const SOURCE: ForgeAttachmentSource = {
  pr: { forge: 'github', host: 'github.com', namespace: 'acme', repo: 'widget', number: 4 },
  backend: 'home',
  webBase: 'https://github.com/acme/widget/pull/4',
};
const GH = 'https://github.com/acme/widget';
const KEY = composeWorkspaceKey('home', prKey(SOURCE.pr));
const WORKSPACE = { backend: 'home', path: '/work/widget' } as unknown as WorkspaceRef;
const HEAD = 'a'.repeat(40);
const NEXT_HEAD = 'e'.repeat(40);
const BASE = 'b'.repeat(40);
const SHA = '0123456789abcdef0123456789abcdef01234567';
const OID = 'c'.repeat(40);
const PERMALINK = `${GH}/blob/${SHA}/src/x.ts#L3-L5`;
const LINES = linesFactKey(SHA, 'src/x.ts', 3, 5);

let calls: { keys: readonly string[]; answer: (facts: ForgeFact[]) => void }[] = [];

/** A store holding nothing, whose requests wait for `answer`. */
function freshFacts(): void {
  __resetForgeFactsForTest();
  calls = [];
  const call: ResolveForgeFactsCall = (_b, _w, _p, _r, keys) => new Promise((resolve) => calls.push({ keys: [...keys], answer: (facts) => resolve({ facts }) }));
  __setResolveForgeFactsForTest(call);
  noteForgeFactsWorkspace(KEY, WORKSPACE);
}

beforeEach(() => {
  freshFacts();
  resetCodeSpanCacheForTest();
  __resetStreamdownCodeHostForTest();
  setBindingMock('HighlightCode', async ({ lang }: { lang: string }) => ({ lang, lines: [], truncated: false }));
  setBindingMock('HighlightSchemaVersion', async () => 'hv-test');
  setBindingMock('HighlightClassNames', async () => ['none']);
  setBindingMock('HighlightPatch', async () => ({ lines: [] }));
  document.body.innerHTML = '';
});

afterEach(() => {
  __resetForgeFactsForTest();
  __resetMediaBlobCacheForTest();
  __resetImageTiersForTest();
  vi.unstubAllGlobals();
  resetAttachmentTransferMocks();
  resetBindingMocks();
  document.body.innerHTML = '';
});

/** The PR's snapshot with this description, at `head`. */
function applySnapshot(body: string, head = HEAD): void {
  observePRFacts(KEY, SOURCE.pr, {
    detail: { number: 4, body, url: SOURCE.webBase, latestReviews: [], headSHA: head, baseRefSHA: BASE, headRefName: 'feature/x', baseRefName: 'main' } as unknown as PRDetail,
    threads: [],
  });
}

const imageFound = (key: string, extra: Partial<ForgeFact> = {}): ForgeFact => ({ key, status: 'found', oid: OID, width: 640, height: 480, mime: 'image/png', size: 100, ...extra });

function factFor(key: string): ForgeFact {
  if (key.startsWith('image:')) return imageFound(key);
  if (key.startsWith('lines:')) return { key, status: 'found', oid: OID, text: 'const one = 1;\nconst two = 2;\nconst three = 3;', path: 'src/x.ts' };
  return { key, status: 'unavailable' };
}

async function frame(): Promise<void> {
  await new Promise((resolve) => requestAnimationFrame(() => resolve(null)));
}

async function answer(facts: (key: string) => ForgeFact = factFor): Promise<void> {
  for (const call of calls.splice(0)) call.answer(call.keys.map(facts));
  for (let i = 0; i < 6; i++) await tick();
  await frame();
}

const sourceWithFacts = (): ForgeAttachmentSource => ({ ...SOURCE, facts: forgeFactReader(KEY) });

function hostOf(width = 600): HTMLElement {
  const host = document.createElement('div');
  host.style.width = `${width}px`;
  document.body.appendChild(host);
  return host;
}

function mount(body: string, host = hostOf(), className = 'review-prose', facts = true) {
  const rendered = render(ChatMarkdown, {
    target: host,
    props: { source: body, pathRefs: EMPTY_PATH_REFS, embeddedHtml: true, class: className },
    context: new Map([[FORGE_ATTACHMENT_SOURCE_CONTEXT, () => (facts ? sourceWithFacts() : SOURCE)]]),
  });
  return { ...rendered, root: () => host.querySelector('.markdown-body')! };
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

/** A PNG of this size, as the backend would serve one. */
async function png(width: number, height: number): Promise<Blob> {
  const canvas = new OffscreenCanvas(width, height);
  const context = canvas.getContext('2d')!;
  context.fillStyle = '#2a6';
  context.fillRect(0, 0, width, height);
  return canvas.convertToBlob({ type: 'image/png' });
}

/** Holds every attachment transfer until the returned function runs. */
function holdTransfers(): () => void {
  let release: () => void = () => {};
  const held = new Promise<void>((resolve) => (release = resolve));
  const fetch = globalThis.fetch;
  vi.stubGlobal('fetch', async (input: RequestInfo | URL, init?: RequestInit) => {
    if (String(input instanceof Request ? input.url : input).includes('/attachments/')) await held;
    return fetch(input, init);
  });
  return release;
}

async function painted(root: Element): Promise<HTMLImageElement> {
  await waitFor(() => expect(root.querySelector('img[src^="blob:"]')).not.toBeNull(), { timeout: 3000 });
  const img = root.querySelector<HTMLImageElement>('img[src^="blob:"]')!;
  await img.decode();
  await frame();
  return img;
}

type Rect = { x: number; y: number; width: number; height: number };
const rectOf = (element: Element): Rect => {
  const { x, y, width, height } = element.getBoundingClientRect();
  return { x: Math.round(x * 2) / 2, y: Math.round(y * 2) / 2, width: Math.round(width * 2) / 2, height: Math.round(height * 2) / 2 };
};

/** The tops of `element` sampled every frame until `stop` resolves: the distinct values, in order. */
function sampleTops(element: () => Element | null, stop: Promise<unknown>): Promise<number[]> {
  const tops: number[] = [];
  let done = false;
  void stop.finally(() => (done = true));
  return new Promise((resolve) => {
    const step = () => {
      const at = element();
      if (at) {
        const top = Math.round(at.getBoundingClientRect().top * 2) / 2;
        if (tops.at(-1) !== top) tops.push(top);
      }
      if (done) resolve(tops);
      else requestAnimationFrame(step);
    };
    step();
  });
}

const BODY = 'Intro paragraph.\n\n![shot](docs/a.png)\n\nAfter paragraph.';
const after = (root: Element) => [...root.querySelectorAll('p')].find((p) => p.textContent === 'After paragraph.') ?? null;

describe('a repository image', () => {
  it.each(['review-prose', ''])('paints at its fact size from the first frame, and the bytes move nothing (class %j)', async (className) => {
    const blob = await png(640, 480);
    const mint = mockGitBlobImage(() => ({ blob, width: 640, height: 480 }));
    const land = holdTransfers();
    applySnapshot(BODY);
    await answer();
    const view = mount(BODY, hostOf(), className);
    const root = view.root();
    const box = root.querySelector('[data-streamdown-image-box] > [role="img"]')!;
    expect(box).not.toBeNull();
    const reserved = rectOf(box);
    const below = rectOf(after(root)!);
    // 640x480 scaled into the 600px column.
    expect(reserved.width).toBe(600);
    expect(reserved.height).toBe(450);
    land();
    const img = await painted(root);
    expect(rectOf(img)).toEqual(reserved);
    expect(rectOf(after(root)!)).toEqual(below);
    expect(mint).toHaveBeenCalledTimes(1);
    expect(mint.mock.calls[0]!.slice(0, 2)).toEqual([WORKSPACE, OID]);
  });

  it('changes its geometry once when the fact arrives after mount, and not again for the bytes', async () => {
    mockGitBlobImage(async () => ({ blob: await png(640, 480), width: 640, height: 480 }));
    applySnapshot(BODY);
    const view = mount(BODY);
    const root = view.root();
    // Asked: the loading chip, with no size to reserve.
    await waitFor(() => expect(root.querySelector('[data-streamdown-image-loading]')).not.toBeNull());
    const intro = root.querySelector('p')!;
    const below = after(root)!;
    let finished: () => void = () => {};
    const stop = new Promise<void>((resolve) => (finished = resolve));
    const tops = sampleTops(() => after(root), stop);
    await frame();
    await answer();
    await painted(root);
    await frame();
    finished();
    expect(await tops).toHaveLength(2);
    // Only the image's block rebuilt.
    expect(root.querySelector('p')).toBe(intro);
    expect(after(root)).toBe(below);
  });

  it('keeps an authored size through asked, found and painted', async () => {
    mockGitBlobImage(async () => ({ blob: await png(640, 480), width: 640, height: 480 }));
    const body = 'Intro paragraph.\n\n<img src="docs/a.png" width="320" height="240" alt="shot">\n\nAfter paragraph.';
    applySnapshot(body);
    const view = mount(body);
    const root = view.root();
    await waitFor(() => expect(root.querySelector('[data-streamdown-image-box]')).not.toBeNull());
    const reserved = rectOf(root.querySelector('[data-streamdown-image-box] > [role="img"]')!);
    expect([reserved.width, reserved.height]).toEqual([320, 240]);
    let finished: () => void = () => {};
    const stop = new Promise<void>((resolve) => (finished = resolve));
    const tops = sampleTops(() => after(root), stop);
    await answer();
    const img = await painted(root);
    finished();
    expect(await tops).toHaveLength(1);
    expect(rectOf(img)).toEqual(reserved);
  });

  it.each([
    ['inline', BODY],
    ['by reference', 'Intro paragraph.\n\n![shot][s]\n\nAfter paragraph.\n\n[s]: docs/a.png'],
    ['in HTML', 'Intro paragraph.\n\n<p><img src="docs/a.png" alt="shot"></p>\n\nAfter paragraph.'],
  ])('shows each state %s', async (_name, body) => {
    const states: [string, ForgeFact | null][] = [
      ['lfs', { key: '', status: 'found', oid: OID, lfs: true }],
      ['absent', { key: '', status: 'absent' }],
      ['unavailable', { key: '', status: 'unavailable' }],
    ];
    const shownAs = (root: Element) => [...root.querySelectorAll('[data-streamdown-image-blocked]')].map((chip) => [chip.textContent, chip.getAttribute('title')]);
    const unread = shownAs(mount(body, hostOf(), 'review-prose', false).root());
    expect(unread).toHaveLength(1);
    for (const [state, fact] of states) {
      freshFacts();
      document.body.innerHTML = '';
      applySnapshot(body);
      await answer((key) => ({ ...fact!, key }));
      const root = mount(body).root();
      await frame();
      const chip = root.querySelector('[data-repository-image]');
      if (state === 'unavailable') {
        // As it renders without facts.
        expect(chip).toBeNull();
        expect(shownAs(root)).toEqual(unread);
        continue;
      }
      expect(chip?.getAttribute('data-repository-image')).toBe(state);
      expect(chip?.textContent).toContain('Image unavailable: shot');
      const link = root.querySelector('[data-forge-attachment-fallback]');
      if (state === 'lfs') {
        expect(chip?.textContent).toContain('Git LFS');
        expect(link?.getAttribute('href')).toBe(`${GH}/blob/${HEAD}/docs/a.png`);
      } else expect(link).toBeNull();
    }
  });

  it('fetches nothing for a body that never mounts, asks no fact per mount, and fetches once for two', async () => {
    const mint = mockGitBlobImage(async () => ({ blob: await png(64, 48), width: 64, height: 48 }));
    applySnapshot(BODY);
    await answer();
    await frame();
    expect(mint).not.toHaveBeenCalled();
    const first = mount(BODY).root();
    const second = mount(BODY).root();
    await painted(first);
    await painted(second);
    expect(mint).toHaveBeenCalledTimes(1);
    expect(calls).toHaveLength(0);
  });

  it('paints a remount in the same frame, with no box first', async () => {
    mockGitBlobImage(async () => ({ blob: await png(64, 48), width: 64, height: 48 }));
    applySnapshot(BODY);
    await answer();
    const first = mount(BODY);
    await painted(first.root());
    first.unmount();
    const again = mount(BODY);
    flushSync();
    const root = again.root();
    expect(root.querySelector('[data-streamdown-image-box]')).toBeNull();
    expect(root.querySelector('img[src^="blob:"]')).not.toBeNull();
  });

  it('paints nothing later and holds nothing when it unmounts mid-fetch', async () => {
    const mint = mockGitBlobImage(async () => ({ blob: await png(64, 48), width: 64, height: 48 }));
    const land = holdTransfers();
    applySnapshot(BODY);
    await answer();
    const host = hostOf();
    const view = mount(BODY, host);
    await waitFor(() => expect(mint).toHaveBeenCalledTimes(1));
    view.unmount();
    land();
    await waitFor(() => expect(mediaBlobCacheStats().loading).toBe(0));
    await frame();
    expect(host.querySelector('img')).toBeNull();
    expect(mediaBlobCacheStats().retained).toBe(0);
  });

  it('lexes only the image block on a push that keeps the file, and fetches nothing new', async () => {
    const mint = mockGitBlobImage(async () => ({ blob: await png(64, 48), width: 64, height: 48 }));
    const body = `Intro paragraph.\n\n![shot](docs/a.png)\n\nSee ${PERMALINK}\n\nAfter paragraph.`;
    applySnapshot(body);
    await answer();
    const root = mount(body).root();
    const img = await painted(root);
    expect(img.title).toBe(`${GH}/blob/${HEAD}/docs/a.png`);
    const intro = root.querySelector('p')!;
    const snippet = root.querySelector('.md-code-snippet')!;
    const below = after(root)!;
    expect(snippet).not.toBeNull();
    for (let i = 0; i < 3; i++) await new Promise((resolve) => setTimeout(resolve, 50));

    const lexes = recordLexes();
    try {
      applySnapshot(body, NEXT_HEAD);
      // The image's key at the new head; the permalink's names its commit.
      expect(calls.flatMap((call) => call.keys).filter((key) => !key.startsWith('commit:'))).toEqual([imageFactKey(NEXT_HEAD, 'docs/a.png')]);
      await answer();
      await waitFor(() => expect(root.querySelector('img')?.title).toBe(`${GH}/blob/${NEXT_HEAD}/docs/a.png`));
      await frame();
      expect(lexes.sources.length).toBeGreaterThan(0);
      // The image's block and its alt text; nothing of the other blocks.
      expect(lexes.sources.filter((source) => !source.includes('docs/a.png') && source !== 'shot')).toEqual([]);
    } finally {
      lexes.restore();
    }
    expect(root.querySelector('p')).toBe(intro);
    expect(root.querySelector('.md-code-snippet')).toBe(snippet);
    expect(after(root)).toBe(below);
    // The same blob: painted from the cache, never a box.
    expect(root.querySelector('[data-streamdown-image-box]')).toBeNull();
    expect(root.querySelector('img[src^="blob:"]')).not.toBeNull();
    expect(mint).toHaveBeenCalledTimes(1);
  });

  it('counts what it shows in find, in every state', async () => {
    const body = 'Intro paragraph.\n\n![shot words](docs/a.png)\n\n<img src="docs/b.png" alt="html words">\n\nAfter paragraph.';
    const shown = (runs: readonly string[]) => runs.map((run) => foldFindText(run).replace(/\s+/g, ' ').trim()).filter(Boolean);
    mockGitBlobImage(() => new Promise(() => {}));
    for (const fact of [null, imageFound(''), { key: '', status: 'absent' } as ForgeFact, { key: '', status: 'found', oid: OID, lfs: true } as ForgeFact]) {
      freshFacts();
      document.body.innerHTML = '';
      applySnapshot(body);
      if (fact) await answer((key) => ({ ...fact, key }));
      const root = mount(body).root();
      await frame();
      const projected = markdownFindText(body, { embeddedHtml: true, forgeSource: SOURCE, facts: () => forgeFactReader(KEY) });
      expect(shown(projected.runs), JSON.stringify(fact)).toEqual(shown(findTextSegments(root)));
    }
  });
});

describe('the box before the bytes, from the mint', () => {
  it('reserves a forge attachment at the size its mint answers', async () => {
    const blob = await png(400, 300);
    let ticket = 0;
    const fetch = globalThis.fetch;
    vi.stubGlobal('fetch', (input: RequestInfo | URL, init?: RequestInit) =>
      String(input).includes('/attachments/forge/') ? Promise.resolve(new Response(blob, { status: 200, headers: { 'Content-Type': 'image/png' } })) : fetch(input, init),
    );
    const land = holdTransfers();
    setBindingMock('FetchForgeAttachment', async () => ({
      url: `/attachments/forge/tok${++ticket}?ticket=t`, mimeType: 'image/png', kind: 'image', sizeBytes: blob.size, filename: 'shot.png',
      width: 400, height: 300, originalWidth: 400, originalHeight: 300, derived: false,
    }));
    const asset = 'https://github.com/user-attachments/assets/4f0b0b1e-1111-2222-3333-444455556666';
    const body = `Intro paragraph.\n\n![shot](${asset})\n\nAfter paragraph.`;
    const host = hostOf();
    render(ChatMarkdown, { target: host, props: { source: body, pathRefs: EMPTY_PATH_REFS, embeddedHtml: true, class: 'review-prose' }, context: new Map([[FORGE_ATTACHMENT_SOURCE_CONTEXT, () => SOURCE]]) });
    const root = host.querySelector('.markdown-body')!;
    await waitFor(() => expect(root.querySelector('[data-streamdown-image-box]')).not.toBeNull());
    const reserved = rectOf(root.querySelector('[data-streamdown-image-box] > [role="img"]')!);
    const below = rectOf(after(root)!);
    expect([reserved.width, reserved.height]).toEqual([400, 300]);
    land();
    const img = await painted(root);
    expect(rectOf(img)).toEqual(reserved);
    expect(rectOf(after(root)!)).toEqual(below);
  });

  it('reserves a local image at the size its mint answers', async () => {
    const blob = await png(400, 300);
    mockLocalImage(() => ({ blob, width: 400, height: 300 }));
    const land = holdTransfers();
    const host = hostOf();
    host.className = 'markdown-body';
    const href = buildLocalImageHref('/workspace/shot.png', '/workspace');
    render(StreamdownImageHost, { target: host, props: { token: { type: 'image', raw: `![shot](${href})`, href, title: null, text: 'shot', tokens: [] }, src: href, backend: 'home' } });
    const below = document.createElement('p');
    below.textContent = 'After paragraph.';
    host.appendChild(below);
    await waitFor(() => expect(host.querySelector('[data-streamdown-image-box]')).not.toBeNull());
    const reserved = rectOf(host.querySelector('[data-streamdown-image-box] > [role="img"]')!);
    const belowAt = rectOf(below);
    expect([reserved.width, reserved.height]).toEqual([400, 300]);
    land();
    const img = await painted(host);
    expect(rectOf(img)).toEqual(reserved);
    expect(rectOf(below)).toEqual(belowAt);
  });
});

describe('a permalink snippet', () => {
  const body = `Intro paragraph.\n\nSee ${PERMALINK} for it.\n\nAfter paragraph.`;

  it('frames the range while asked, at the height the lines fill, numbered from the first', async () => {
    applySnapshot(body);
    expect(calls.flatMap((call) => call.keys)).toContain(LINES);
    const root = mount(body).root();
    await waitFor(() => expect(root.querySelector('.md-code-snippet')).not.toBeNull());
    const snippet = () => root.querySelector('.md-code-snippet')!;
    const pending = rectOf(snippet());
    const below = rectOf(after(root)!);
    expect(snippet().querySelector('.md-code-line-numbers')?.textContent).toBe('3\n4\n5');
    await answer();
    await waitFor(() => expect(snippet().querySelector('pre')?.textContent).toContain('const two = 2;'));
    await frame();
    expect(rectOf(snippet())).toEqual(pending);
    // The first number sits on the first line.
    const gutter = snippet().querySelector('.md-code-line-numbers')!;
    const pre = snippet().querySelector('pre')!;
    const contentTop = (element: Element) => {
      const style = getComputedStyle(element);
      return element.getBoundingClientRect().top + Number.parseFloat(style.paddingTop) + Number.parseFloat(style.borderTopWidth);
    };
    expect(contentTop(gutter)).toBeCloseTo(contentTop(pre), 1);
    expect(getComputedStyle(gutter).lineHeight).toBe(getComputedStyle(pre).lineHeight);
    expect(rectOf(after(root)!)).toEqual(below);
    const header = snippet().querySelector('p')!;
    expect(header.textContent).toBe(`widget/src/x.tsLines 3 to 5 in ${SHA.slice(0, 7)}`);
    expect([...header.querySelectorAll('a')].map((a) => a.getAttribute('href'))).toEqual([PERMALINK, `${GH}/commit/${SHA}`]);
    // Split around the block: the sentence's two halves are paragraphs.
    const texts = [...root.querySelectorAll(':scope > p, :scope > * > p')].map((p) => p.textContent);
    expect(texts).toContain('See');
    expect(texts).toContain('for it.');
  });

  it.each([
    ['an extension the highlighter does not know', 'tools/gen.zzq', 1],
    ['no extension', 'bin/run', 0],
  ])('shows a file with %s as numbered plain text, asking once at most and reporting nothing', async (_name, path, asks) => {
    const highlight = setBindingMock('HighlightCode', async () => ({ lang: 'plaintext', lines: [], truncated: false }));
    const warned = vi.spyOn(console, 'warn');
    const failed = vi.spyOn(console, 'error');
    const link = `${GH}/blob/${SHA}/${path}#L7-L8`;
    applySnapshot(`See ${link}`);
    await answer((key) => (key.startsWith('lines:') ? { key, status: 'found', oid: OID, text: 'alpha();\nbeta();', path } : { key, status: 'unavailable' }));
    const root = mount(`See ${link}`).root();
    await waitFor(() => expect(root.querySelector('.md-code-snippet pre')?.textContent).toBe('alpha();\nbeta();'));
    // Long enough for a retry or a re-highlight to show.
    for (let i = 0; i < 5; i++) await new Promise((resolve) => setTimeout(resolve, 50));
    const snippet = root.querySelector('.md-code-snippet')!;
    expect(snippet.querySelector('.md-code-line-numbers')?.textContent).toBe('7\n8');
    expect(snippet.querySelector('pre code')?.textContent).toBe('alpha();\nbeta();');
    expect(snippet.querySelector('pre code [class]')).toBeNull();
    expect(snippet.querySelector('[data-render-failed]')).toBeNull();
    expect(highlight).toHaveBeenCalledTimes(asks);
    expect(getToasts()).toEqual([]);
    expect(warned).not.toHaveBeenCalled();
    expect(failed).not.toHaveBeenCalled();
  });

  it('renders the same snippet on the component and compact static paths', async () => {
    applySnapshot(body);
    await answer();
    const shape = (root: Element) => {
      const snippet = root.querySelector('.md-code-snippet');
      return snippet && {
        header: snippet.querySelector('p')?.textContent,
        links: [...snippet.querySelectorAll('p a')].map((a) => a.getAttribute('href')),
        numbers: snippet.querySelector('.md-code-line-numbers')?.textContent,
        code: snippet.querySelector('pre')?.textContent,
        unwrapped: snippet.querySelector('[data-streamdown-code]')?.hasAttribute('data-code-unwrapped'),
      };
    };
    const shapes = [];
    for (const streaming of [false, true]) {
      const { container } = render(ForgeAttachmentChatHarness, { props: { source: `See ${PERMALINK}`, forgeSource: sourceWithFacts(), streaming } });
      await waitFor(() => expect(container.querySelector('.md-code-snippet pre')?.textContent).toContain('const two'));
      shapes.push(shape(container));
    }
    expect(shapes[0]).not.toBeNull();
    expect(shapes[1]).toEqual(shapes[0]);
  });

  it('counts what it shows in find, asked and found', async () => {
    applySnapshot(body);
    const shown = (runs: readonly string[]) => runs.map((run) => foldFindText(run).replace(/\s+/g, ' ').trim()).filter(Boolean);
    for (const step of ['asked', 'found']) {
      if (step === 'found') await answer();
      document.body.innerHTML = '';
      const root = mount(body).root();
      await waitFor(() => expect(root.querySelector('.md-code-snippet')).not.toBeNull());
      await frame();
      const projected = markdownFindText(body, { embeddedHtml: true, forgeSource: SOURCE, facts: () => forgeFactReader(KEY) });
      expect(shown(projected.runs), step).toEqual(shown(findTextSegments(root)));
    }
  });
});

describe('a PR frame for mounted bodies', () => {
  const DESCRIPTION = `Intro paragraph.\n\n![shot](docs/a.png)\n\nSee ${PERMALINK}\n\nAfter paragraph.`;
  const FIRST = 'First comment, see #1 and ![b](docs/b.png).';
  const SECOND = 'Second comment.\n\nWith two blocks.';

  function frameOf(comments: readonly string[]): void {
    observePRFacts(KEY, SOURCE.pr, {
      detail: { number: 4, body: DESCRIPTION, url: SOURCE.webBase, latestReviews: [], headSHA: HEAD, baseRefSHA: BASE, headRefName: 'feature/x', baseRefName: 'main' } as unknown as PRDetail,
      threads: [{ id: 't', comments: comments.map((body, index) => ({ id: `c${index}`, body })) } as never],
    });
  }

  it('costs no scan and no lex when it changes nothing, and one scan and lexes of that body alone when it changes one comment', async () => {
    mockGitBlobImage(async () => ({ blob: await png(64, 48), width: 64, height: 48 }));
    frameOf([FIRST, SECOND]);
    await answer();
    const description = mount(DESCRIPTION);
    const first = mount(FIRST);
    const second = mount(SECOND);
    await painted(description.root());
    await painted(first.root());
    for (let i = 0; i < 3; i++) await new Promise((resolve) => setTimeout(resolve, 50));
    expect(calls).toHaveLength(0);

    const lexes = recordLexes();
    try {
      forgeFactsCounters.scans = 0;
      for (let i = 0; i < 3; i++) frameOf([FIRST, SECOND]);
      await answer();
      for (let i = 0; i < 3; i++) await new Promise((resolve) => setTimeout(resolve, 50));
      expect(forgeFactsCounters.scans).toBe(0);
      expect(calls).toHaveLength(0);
      expect(lexes.sources).toEqual([]);

      const EDITED = 'Second comment, edited.\n\nWith two blocks.';
      frameOf([FIRST, EDITED]);
      await second.rerender({ source: EDITED });
      await answer();
      await waitFor(() => expect(second.root().textContent).toContain('edited'));
      for (let i = 0; i < 3; i++) await new Promise((resolve) => setTimeout(resolve, 50));
      expect(forgeFactsCounters.scans).toBe(1);
      expect(lexes.sources.length).toBeGreaterThan(0);
      // Only the edited body. Its blocks from the edit on lex again: the
      // committed renderer keeps the unchanged prefix of a body, not blocks
      // after a change.
      expect(lexes.sources.filter((source) => !EDITED.includes(source))).toEqual([]);
      expect(lexes.sources).toContain('Second comment, edited.');
    } finally {
      lexes.restore();
    }
  });
});

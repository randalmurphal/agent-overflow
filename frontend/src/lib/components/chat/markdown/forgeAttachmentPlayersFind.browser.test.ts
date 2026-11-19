// In real Chromium: a forge attachment that resolves to a video or audio
// player ends find's run, as an image does, however the author wrote it
// (a Markdown image, an inline `<video>`, a `<video>` in an HTML block) and
// on both render paths, and markdownFindText projects the same runs. The
// text on either side of a player never joins into one match.

import { render, waitFor } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import '../../../../app.css';
import ForgeAttachmentChatHarness from '../ForgeAttachmentChatHarness.svelte';
import { findTextSegments } from '../../../utils/findHighlights';
import { countFindMatches, findNeedle, foldFindText } from '../../../utils/findMatch';
import { __resetMediaBlobCacheForTest } from '../../../utils/mediaBlobCache';
import type { ForgeAttachmentSource } from '../../../utils/forgeAttachments';
import { resetBindingMocks, setBindingMock } from '../../../../test/mocks/bindings-app';
import { markdownFindText } from './markdownFindText';

const GITHUB: ForgeAttachmentSource = {
  pr: { forge: 'github', host: 'github.com', namespace: 'octo', repo: 'widget', number: 7 },
  backend: 'home',
  webBase: 'https://github.com/octo/widget/pull/7',
};
const GITLAB: ForgeAttachmentSource = {
  pr: { forge: 'gitlab', host: 'gitlab.com', namespace: 'group', repo: 'widget', number: 3 },
  backend: 'home',
  webBase: 'https://gitlab.com/group/widget/-/merge_requests/3',
};
const ASSETS: Record<'github' | 'gitlab', string> = {
  github: 'https://github.com/user-attachments/assets/4f0b0b1e-1111-2222-3333-444455556666',
  gitlab: '/uploads/0123456789abcdef0123456789abcdef/clip.mp4',
};

type Kind = 'video' | 'audio';

// The forge answers every attachment as `kind`.
function stage(kind: Kind): void {
  const mimeType = kind === 'video' ? 'video/mp4' : 'audio/mpeg';
  const fetch = globalThis.fetch;
  vi.stubGlobal('fetch', (input: RequestInfo | URL, init?: RequestInit) =>
    String(input instanceof Request ? input.url : input).includes('/attachments/forge/')
      ? Promise.resolve(new Response('media bytes', { status: 200, headers: { 'Content-Type': mimeType } }))
      : fetch(input, init),
  );
  let ticket = 0;
  setBindingMock('FetchForgeAttachment', async () => ({
    url: `/attachments/forge/tok${++ticket}?ticket=t`,
    mimeType,
    kind,
    sizeBytes: 11,
    filename: kind === 'video' ? 'clip.mp4' : 'clip.mp3',
  }));
}

let host: HTMLElement;

beforeEach(() => {
  document.body.innerHTML = '';
  host = document.createElement('div');
  host.style.width = '600px';
  document.body.appendChild(host);
  setBindingMock('HighlightSchemaVersion', async () => 'hv-test');
  setBindingMock('HighlightClassNames', async () => ['none']);
  setBindingMock('HighlightPatch', async () => ({ lines: [] }));
});

afterEach(() => {
  __resetMediaBlobCacheForTest();
  resetBindingMocks();
  vi.unstubAllGlobals();
});

const count = (runs: readonly string[], query: string): number =>
  runs.reduce((total, run) => total + countFindMatches(run, findNeedle(query)), 0);

const SHAPES: [string, (asset: string) => string][] = [
  ['a Markdown image', (asset) => `before ![clip](${asset}) after`],
  ['an inline <video>', (asset) => `before <video src="${asset}"></video> after`],
  ['a <video> in an HTML block', (asset) => `<p>before <video src="${asset}"></video> after</p>`],
];

describe.each([
  ['GitHub', GITHUB],
  ['GitLab', GITLAB],
] as const)('a %s attachment player', (_forge, forgeSource) => {
  describe.each(SHAPES)('written as %s', (_shape, write) => {
    it.each([
      ['video', false],
      ['video', true],
      ['audio', false],
      ['audio', true],
    ] as const)('ends the find run as a %s player (streaming %s)', async (kind, streaming) => {
      stage(kind);
      const source = `${write(ASSETS[forgeSource.pr.forge as 'github' | 'gitlab'])}\n\nlast line`;
      render(ForgeAttachmentChatHarness, { target: host, props: { source, forgeSource, streaming } });
      await waitFor(() => expect(host.querySelector(`[data-forge-attachment-${kind}] ${kind}`)).toBeTruthy());
      const dom = findTextSegments(host.querySelector('.markdown-body')!);
      const projected = markdownFindText(source, { embeddedHtml: true, forgeSource }).runs;
      expect(dom.filter((run) => run.trim() !== '')).toEqual(projected.map(foldFindText).filter((run) => run.trim() !== ''));
      expect([count(dom, 'before'), count(dom, 'after'), count(dom, 'before after')]).toEqual([1, 1, 0]);
    });
  });
});

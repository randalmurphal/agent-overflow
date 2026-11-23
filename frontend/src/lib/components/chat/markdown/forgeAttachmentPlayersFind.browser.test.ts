// In real Chromium: a forge attachment ends find's run in every state it
// shows (loading, a picture, a video or audio player, a file chip, a
// failure), however the author wrote it (a Markdown image, an inline
// `<video>`, a `<video>` in an HTML block) and on both render paths, and
// markdownFindText projects the same runs. The text on either side of an
// attachment never joins into one match.

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

type Outcome = 'loading' | 'failed' | 'image' | 'video' | 'audio' | 'file';

// What the attachment shows in each outcome.
const SHOWN: Record<Outcome, string> = {
  loading: '[data-forge-attachment-loading]',
  failed: '[data-forge-attachment-error]',
  image: '[data-streamdown-image] img',
  video: '[data-forge-attachment-video] video',
  audio: '[data-forge-attachment-audio] audio',
  file: '[data-forge-attachment-file]',
};

const svg = '<svg xmlns="http://www.w3.org/2000/svg" width="80" height="20"><rect width="80" height="20" fill="green"/></svg>';

// The forge answers every attachment with `outcome`.
function stage(outcome: Outcome): void {
  if (outcome === 'loading') {
    setBindingMock('FetchForgeAttachment', () => new Promise(() => {}));
    return;
  }
  if (outcome === 'failed') {
    setBindingMock('FetchForgeAttachment', async () => {
      throw new Error('forge refused the attachment');
    });
    return;
  }
  const mimeType = { image: 'image/svg+xml', video: 'video/mp4', audio: 'audio/mpeg', file: 'application/pdf' }[outcome];
  const body = outcome === 'image' ? svg : 'media bytes';
  const fetch = globalThis.fetch;
  vi.stubGlobal('fetch', (input: RequestInfo | URL, init?: RequestInit) =>
    String(input instanceof Request ? input.url : input).includes('/attachments/forge/')
      ? Promise.resolve(new Response(body, { status: 200, headers: { 'Content-Type': mimeType } }))
      : fetch(input, init),
  );
  let ticket = 0;
  setBindingMock('FetchForgeAttachment', async () => ({
    url: `/attachments/forge/tok${++ticket}?ticket=t`,
    mimeType,
    kind: outcome,
    sizeBytes: body.length,
    filename: `clip.${{ image: 'svg', video: 'mp4', audio: 'mp3', file: 'pdf' }[outcome]}`,
    ...(outcome === 'image' ? { width: 80, height: 20, originalWidth: 80, originalHeight: 20, derived: false } : {}),
  }));
}
const OUTCOMES: Outcome[] = ['loading', 'failed', 'image', 'video', 'audio', 'file'];

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
] as const)('a %s attachment', (_forge, forgeSource) => {
  describe.each(SHAPES)('written as %s', (_shape, write) => {
    it.each(OUTCOMES.flatMap((outcome) => [[outcome, false], [outcome, true]] as const))('ends the find run when it shows %s (streaming %s)', async (outcome, streaming) => {
      stage(outcome);
      const source = `${write(ASSETS[forgeSource.pr.forge as 'github' | 'gitlab'])}\n\nlast line`;
      render(ForgeAttachmentChatHarness, { target: host, props: { source, forgeSource, streaming } });
      await waitFor(() => expect(host.querySelector(SHOWN[outcome])).toBeTruthy());
      const dom = findTextSegments(host.querySelector('.markdown-body')!);
      const projected = markdownFindText(source, { embeddedHtml: true, forgeSource }).runs;
      expect(dom.filter((run) => run.trim() !== '')).toEqual(projected.map(foldFindText).filter((run) => run.trim() !== ''));
      expect([count(dom, 'before'), count(dom, 'after'), count(dom, 'before after')]).toEqual([1, 1, 0]);
    });
  });
});

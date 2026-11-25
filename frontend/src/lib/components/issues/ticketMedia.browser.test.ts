// Real-Chromium check of an image in a ticket's text scrolled away and
// back: it lets its bytes go while its box stays the size it painted at, so
// the text after it does not move, and it paints again on its return.

import { afterEach, describe, expect, it } from 'vitest';
import { mount, unmount } from 'svelte';
import '../../../app.css';
import TicketMarkdown from './TicketMarkdown.svelte';
import { TRACKER_MEDIA_CONTEXT, type TrackerMediaReader } from '../chat/markdown/trackerMediaContext';
import type { MarkdownImageSource, MarkdownImageVariant } from '../../utils/markdownImageSource';
import { waitFor } from '../../../test/helpers/browserFrames';

const mounted: { app: object; host: HTMLElement }[] = [];

afterEach(() => {
  for (const { app, host } of mounted.splice(0)) {
    unmount(app);
    host.remove();
  }
});

/** A 400 by 300 PNG as a data URL. */
function picture(): string {
  const canvas = document.createElement('canvas');
  canvas.width = 400;
  canvas.height = 300;
  const g = canvas.getContext('2d')!;
  g.fillStyle = '#3a7';
  g.fillRect(0, 0, 400, 300);
  return canvas.toDataURL('image/png');
}

describe('an image in ticket text', () => {
  it('releases its bytes once scrolled away, keeping its box, and paints again when scrolled back', async () => {
    const url = picture();
    const holds = { taken: 0, released: 0 };
    const variant: MarkdownImageVariant = {
      url, mimeType: 'image/png', width: 0, height: 0, originalWidth: 0, originalHeight: 0, originalBytes: 1000, derived: false,
    } as MarkdownImageVariant;
    const image: MarkdownImageSource = {
      key: 'browser-img-1',
      filename: 'shot.png',
      acquire: () => {
        holds.taken += 1;
        return { settled: variant, value: Promise.resolve(variant), sized: Promise.resolve(undefined), release: () => { holds.released += 1; } };
      },
      original: async () => new Blob(),
      menuTag: {},
    };
    const read: TrackerMediaReader = (name) => (name === 'shot.png' ? { name, size: 1000, image, save: () => {}, saveLabel: 'Save' } : null);

    const host = document.createElement('div');
    host.style.cssText = 'position: fixed; top: 0; left: 0; width: 600px; height: 400px; overflow-y: auto;';
    document.body.appendChild(host);
    const tail = Array.from({ length: 80 }, (_, i) => `Line ${i + 1} after the picture.`).join('\n\n');
    const app = mount(TicketMarkdown, {
      target: host,
      props: { source: `![shot.png](media:1)\n\n${tail}` },
      context: new Map([[TRACKER_MEDIA_CONTEXT, read]]),
    });
    mounted.push({ app, host });

    const painted = () => host.querySelector<HTMLImageElement>('[data-tracker-media-image] img[src^="data:image/png"]');
    await waitFor(() => (painted()?.naturalWidth ?? 0) === 400, 'the picture to paint');
    const wrapper = host.querySelector<HTMLElement>('[data-tracker-media-image]')!;
    const line = [...host.querySelectorAll('p')].find((p) => p.textContent === 'Line 1 after the picture.')!;
    const before = { box: wrapper.getBoundingClientRect().height, line: line.getBoundingClientRect().top + host.scrollTop };
    expect(before.box).toBeGreaterThan(290);
    expect(holds).toEqual({ taken: 1, released: 0 });

    host.scrollTop = 2000;
    await waitFor(() => holds.released === 1, 'the picture to release its bytes');
    expect(painted()).toBeNull();
    expect(wrapper.querySelector('[data-streamdown-image-box]')).not.toBeNull();
    expect(wrapper.getBoundingClientRect().height).toBe(before.box);
    expect(line.getBoundingClientRect().top + host.scrollTop).toBe(before.line);

    host.scrollTop = 0;
    await waitFor(() => (painted()?.naturalWidth ?? 0) === 400, 'the picture to paint again');
    expect(holds.taken).toBe(2);
    expect(wrapper.getBoundingClientRect().height).toBe(before.box);
  });
});

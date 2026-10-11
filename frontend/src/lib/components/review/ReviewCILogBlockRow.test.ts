import { flushSync, mount, tick, unmount } from 'svelte';
import { afterEach, describe, expect, it } from 'vitest';
import ReviewCILogBlockRow from './ReviewCILogBlockRow.svelte';
import { CI_LOG_BLOCK_CACHE_MAX, CI_LOG_BLOCK_LINES, CILogBlockCache } from '../../stores/ciLogBlocks.svelte';
import { FakeCILog, numberedLines } from '../../../test/helpers/ciLog';

const mounted: { app: object; host: HTMLElement }[] = [];

afterEach(() => {
  for (const { app, host } of mounted.splice(0)) {
    void unmount(app);
    host.remove();
  }
});

function cacheOf(fake: FakeCILog, fetch = fake.fetch): CILogBlockCache {
  const blocks = new CILogBlockCache(fetch);
  blocks.setIndex(fake.version, fake.lines.length, 0);
  return blocks;
}

function mountRow(blocks: CILogBlockCache, from: number, to: number) {
  const host = document.createElement('div');
  document.body.appendChild(host);
  const app = mount(ReviewCILogBlockRow, { target: host, props: { blocks, from, to, linePx: 18 } });
  const entry = { app, host };
  mounted.push(entry);
  return {
    host,
    unmount: () => {
      void unmount(app);
      host.remove();
      mounted.splice(mounted.indexOf(entry), 1);
    },
  };
}

async function settle(): Promise<void> {
  for (let i = 0; i < 6; i += 1) await tick();
  flushSync();
}

describe('<ReviewCILogBlockRow>', () => {
  it('shows its lines of the block, holding the block only while mounted', async () => {
    const fake = new FakeCILog(numberedLines((CI_LOG_BLOCK_CACHE_MAX + 1) * CI_LOG_BLOCK_LINES));
    const blocks = cacheOf(fake);
    const row = mountRow(blocks, 3, 6);
    await settle();
    expect(row.host).toHaveTextContent('line 4 line 5 line 6');
    expect(row.host).not.toHaveTextContent('line 3 ');
    expect(row.host).not.toHaveTextContent('line 7');
    expect(fake.reads).toEqual([[0, CI_LOG_BLOCK_LINES]]);

    const fill = async () => {
      for (let index = 1; index <= CI_LOG_BLOCK_CACHE_MAX; index += 1) {
        const release = blocks.acquire(index);
        await settle();
        release();
      }
    };
    // Held, the block outlives the bound; released, it is the oldest.
    await fill();
    expect(blocks.block(0)).toBeDefined();
    row.unmount();
    await fill();
    expect(blocks.block(0)).toBeUndefined();
  });

  it('shows a block\'s failure in its place and reads it again when it mounts again', async () => {
    const fake = new FakeCILog(numberedLines(5));
    fake.failure = new Error('the downloaded log is gone; refresh to download it again');
    const blocks = cacheOf(fake);
    const first = mountRow(blocks, 0, 5);
    await settle();
    const error = first.host.querySelector('[data-testid="review-ci-log-block-error"]');
    expect(error).toHaveTextContent(
      'Lines 1 to 5 failed to load: the downloaded log is gone; refresh to download it again. Scroll away and back to retry.',
    );
    expect(first.host.querySelector('[data-testid="review-ci-log-block-placeholder"]')).toBeNull();
    expect(fake.reads).toHaveLength(1);

    // Scrolled away and back: the row unmounts and mounts again.
    first.unmount();
    fake.failure = null;
    const second = mountRow(blocks, 0, 5);
    await settle();
    expect(fake.reads).toHaveLength(2);
    expect(second.host.querySelector('[data-testid="review-ci-log-block-error"]')).toBeNull();
    expect(second.host).toHaveTextContent('line 5');
  });

  it('marks a line the backend clipped', async () => {
    const fake = new FakeCILog(['short', 'x'.repeat(100)]);
    const blocks = cacheOf(fake, async (from, count) => ({ ...(await fake.fetch(from, count)), clipped: [1] }));
    const row = mountRow(blocks, 0, 2);
    await settle();
    expect(row.host).toHaveTextContent(`short ${'x'.repeat(100)} … line clipped at 64 KB; Save to file has the whole line`);
    expect(row.host).not.toHaveTextContent('short …');
  });
});

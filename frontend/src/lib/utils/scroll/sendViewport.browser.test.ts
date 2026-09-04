import { expect, it } from 'vitest';
import { createUseStickToBottomController } from './index.svelte';
import { raf, waitFor } from '../../../test/helpers/browserFrames';

it('a viewport-only delivery cannot snap an optimistic send to the bottom', async () => {
  const scroller = document.createElement('div');
  scroller.style.cssText = 'position:fixed;width:400px;height:600px;overflow:auto;overflow-anchor:none;box-sizing:border-box';
  const content = document.createElement('div');
  content.style.height = '1000px';
  scroller.appendChild(content);
  document.body.appendChild(scroller);
  const controller = createUseStickToBottomController({ externalContentGeometry: true });
  const deliver = (height: number, viewportHeight: number) => controller.deliverContentGeometry({
    height, viewportHeight, width: 400, windowMeasured: true, maxFirstMeasureCorrectionPx: 0,
  });
  try {
    controller.attach(scroller, content);
    deliver(1000, 600);
    controller.skipWarmup();
    expect(scroller.scrollTop).toBe(400);
    controller.markStructuralContentPending();
    content.style.height = '1272px';
    deliver(1272, 600);
    await waitFor(() => scroller.scrollTop > 400, 'send motion to start');
    const before = scroller.scrollTop;
    scroller.style.paddingBottom = '205px';
    deliver(1272, 395);
    expect(scroller.scrollTop, 'the viewport delivery must preserve the glide').toBe(before);
    controller.observe('composer-geometry');
    expect(scroller.scrollTop).toBe(before);
    await raf();
    expect(scroller.scrollTop - before).toBeLessThan(30);
    await waitFor(() => Math.abs(scroller.scrollHeight - scroller.clientHeight - scroller.scrollTop) <= 1,
      'the send glide to arrive', 240);
  } finally {
    controller.detach();
    scroller.remove();
  }
});

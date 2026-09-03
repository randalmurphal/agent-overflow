// Real-Chromium tripwire for the selection-tracking button state in
// intent.ts. A native drag-and-drop fires mousedown and then NO mouseup or
// click; a state latched from that pair stayed "held" until the next click,
// and with a caret anywhere near a timeline every spring paused silently
// (bug-report-20260903T221457Z). The button state is read from the pointer
// stream instead, and this proves both halves against the real browser:
// the drag really does swallow the release, and the predicate does not care.
import { describe, expect, it } from 'vitest';
import { userEvent } from 'vitest/browser';
import { createScrollIntent, isSelectingInside } from './intent';

describe('selection tracking survives a native drag-and-drop', () => {
  it('a drag fires no mouseup or click, and isSelectingInside is false afterwards', async () => {
    document.body.innerHTML = `
      <div style="padding:20px">
        <div id="scroller" style="height:100px;overflow:auto">
          <p id="text">click into this text so a caret sits inside the scroller</p>
          <div style="height:1000px"></div>
        </div>
        <div id="src" draggable="true" style="width:80px;height:40px;background:#c33">drag me</div>
        <div id="dst" style="width:200px;height:120px;background:#3c3;margin-top:40px">drop here</div>
      </div>`;
    // Installs the module-level pointer listeners; the machine itself is unused.
    createScrollIntent(new Proxy({}, { get: () => () => 0 }) as never);
    const counts = { mousedown: 0, mouseup: 0, click: 0, dragstart: 0, drop: 0 };
    for (const type of Object.keys(counts) as (keyof typeof counts)[]) {
      document.addEventListener(type, () => { counts[type] += 1; }, { capture: true });
    }
    const scroller = document.getElementById('scroller')!;
    const src = document.getElementById('src')!;
    const dst = document.getElementById('dst')!;
    src.addEventListener('dragstart', (e) => e.dataTransfer?.setData('text/plain', 'x'));
    dst.addEventListener('dragover', (e) => e.preventDefault());
    dst.addEventListener('drop', (e) => e.preventDefault());

    await userEvent.click(document.getElementById('text')!);
    expect(window.getSelection()?.rangeCount, 'a caret exists inside the scroller').toBe(1);
    const before = { ...counts };

    await userEvent.dragAndDrop(src, dst);
    await new Promise((resolve) => setTimeout(resolve, 50));

    expect(counts.dragstart).toBe(1);
    expect(counts.drop).toBe(1);
    expect(counts.mousedown - before.mousedown, 'the drag began with a mousedown').toBe(1);
    expect(counts.mouseup - before.mouseup, 'the browser fires no mouseup after a drag').toBe(0);
    expect(counts.click - before.click, 'and no click').toBe(0);
    expect(isSelectingInside(scroller)).toBe(false);
  });
});

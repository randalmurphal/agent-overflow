// Verify programmatic scrollTop readback in the running WebView2 renderer.
import { connectPage, done, evaluate } from './lib/cdp.mjs';

const writes = [0.1, 0.25, 0.4, 0.5, 0.6, 0.75, 1.1, 1.25, 1.5, 1.75, 2.4];
const page = await connectPage();

let code = 0;
try {
  const result = await evaluate(page, `(() => {
    const scroller = document.createElement('div');
    scroller.style.cssText = 'position:fixed;left:-10000px;top:0;width:100px;height:100px;overflow:auto';
    const content = document.createElement('div');
    content.style.height = '1000px';
    scroller.appendChild(content);
    document.body.appendChild(scroller);
    try {
      const rows = ${JSON.stringify(writes)}.map((requested) => {
        scroller.scrollTop = requested;
        return { requested, readback: scroller.scrollTop };
      });
      return { dpr: devicePixelRatio, userAgent: navigator.userAgent, rows };
    } finally {
      scroller.remove();
    }
  })()`);
  const mismatches = result.rows.filter((row) => row.readback !== Math.round(row.requested));
  console.log(`WebView2 scrollTop contract at DPR ${result.dpr}`);
  console.log(`  ${result.userAgent}`);
  for (const row of result.rows) {
    console.log(`  ${row.requested}px -> ${row.readback}px`);
  }
  if (mismatches.length > 0) {
    throw new Error(
      `scrollTop readback is not whole-CSS-pixel quantized: ${JSON.stringify(mismatches)}`,
    );
  }
} catch (error) {
  console.error(`probe: ${error.message}`);
  code = 1;
}

await done([page], code);

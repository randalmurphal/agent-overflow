// Read-only status for the bounded real-use page monitor.
import { connectPage, done, evaluate } from './lib/cdp.mjs';

const page = await connectPage();
try {
  const state = await evaluate(page, `(() => {
    const monitor = window.__agentOverflowRealUseMonitor_8f4f25b1;
    if (!monitor?.running) return { active: false };
    return {
      active: true,
      version: monitor.version,
      ageMs: Math.round(performance.now() - monitor.startedAtMs),
      sinceCollectMs: Math.round(performance.now() - monitor.lastCollectAtMs),
      visible: monitor.visible,
      focused: monitor.focused,
    };
  })()`);
  console.log(JSON.stringify(state));
} finally {
  await done([page]);
}

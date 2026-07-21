// On-demand renderer memory accounting, callable from the DevTools
// console or a CDP probe as `window.__aoMemoryReport()`. Combines the
// browser-level numbers (measureUserAgentSpecificMemory when the app
// runs cross-origin isolated — see AGENT_OVERFLOW_RENDERER_DIAG — plus
// the Chromium performance.memory heap gauge and a full DOM census)
// with app-level cache accounting so "what is the renderer holding" is
// answerable without a heap snapshot.
//
// This module is reached ONLY through the dynamic-import stub main.ts
// installs as `window.__aoMemoryReport` — nothing here is in the
// startup graph, so the store imports below are plain static imports.
// Rolldown extracts the shared store modules into their own chunks
// (they are also part of the eager graph), which is why several small
// store chunks appear in dist/ alongside this one.
import { threadItemCache } from '../stores/threadItemCache';
import { proposedPlanCacheStats } from '../stores/proposedPlans.svelte';
import { listPanes } from '../stores/panes.svelte';
import { codeSpanCacheStats } from '../components/chat/markdown/codeSpanCache';
import { sizePriorsStats } from './virtual/priors';

export interface MemoryReport {
  at: string;
  crossOriginIsolated: boolean;
  /** performance.measureUserAgentSpecificMemory() result; null when the
   *  app is not cross-origin isolated (the default, non-diag mode). */
  uaMemory: unknown;
  jsHeap: { usedMB: number; totalMB: number } | null;
  dom: { nodes: number; elements: number; texts: number; comments: number };
  panes: Array<{ paneId: string; threadId: string | null; items: number; channelMessages: number }>;
  caches: {
    threadItems: { threads: number; items: number; chars: number };
    codeSpans: { entries: number; approxKeyChars: number };
    sizePriors: { threads: number; rows: number };
    proposedPlans: ReturnType<typeof proposedPlanCacheStats>;
  };
}

function domCensus(): MemoryReport['dom'] {
  let nodes = 0;
  let elements = 0;
  let texts = 0;
  let comments = 0;
  const walker = document.createTreeWalker(document, NodeFilter.SHOW_ALL);
  for (let node: Node | null = walker.currentNode; node; node = walker.nextNode()) {
    nodes += 1;
    switch (node.nodeType) {
      case Node.ELEMENT_NODE:
        elements += 1;
        break;
      case Node.TEXT_NODE:
        texts += 1;
        break;
      case Node.COMMENT_NODE:
        comments += 1;
        break;
    }
  }
  return { nodes, elements, texts, comments };
}

export async function collectMemoryReport(): Promise<MemoryReport> {
  let uaMemory: unknown = null;
  const perf = performance as Performance & {
    measureUserAgentSpecificMemory?: () => Promise<unknown>;
    memory?: { usedJSHeapSize: number; totalJSHeapSize: number };
  };
  const isolated = globalThis.crossOriginIsolated === true;
  if (isolated && typeof perf.measureUserAgentSpecificMemory === 'function') {
    uaMemory = await perf.measureUserAgentSpecificMemory();
  }

  return {
    at: new Date().toISOString(),
    crossOriginIsolated: isolated,
    uaMemory,
    jsHeap: perf.memory
      ? {
          usedMB: Math.round(perf.memory.usedJSHeapSize / 1048576),
          totalMB: Math.round(perf.memory.totalJSHeapSize / 1048576),
        }
      : null,
    dom: domCensus(),
    panes: listPanes().map((pane) => ({
      paneId: pane.paneId,
      threadId: pane.threadId ?? null,
      items: pane.items.length,
      channelMessages: pane.channelMessages.length,
    })),
    caches: {
      threadItems: threadItemCache.stats(),
      codeSpans: codeSpanCacheStats(),
      sizePriors: sizePriorsStats(),
      proposedPlans: proposedPlanCacheStats(),
    },
  };
}

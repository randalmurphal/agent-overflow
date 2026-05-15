import type { Item, Thread } from '../types/models';
import type {
  ActiveOptionSet,
  ClarificationRequest,
  DesignViewport,
  SliderControl,
} from '../types/design';
import { controlsKey, parseDesignAssistantPayloads } from '../utils/designAssistantPayload';
import { LatestDesignOptionSet } from './bindings';

export interface ThreadDesignState {
  readonly pendingClarification: ClarificationRequest | null;
  readonly exposedControls: SliderControl[];
  readonly activeOptionSet: ActiveOptionSet | null;
  readonly designViewport: DesignViewport;
  reset(): void;
  applyAssistantPayloadsForItem(item: Item, thread: Thread | null): void;
  applyDesignOptionsUpdate(getCurrentThread: () => Thread | null, threadId: string): Promise<void>;
  setPendingClarification(request: ClarificationRequest | null): void;
  setExposedControls(controls: SliderControl[]): void;
  setActiveOptionSet(set: ActiveOptionSet | null): void;
  setDesignViewport(viewport: DesignViewport): void;
}

export function createThreadDesignState(): ThreadDesignState {
  let pendingClarification: ClarificationRequest | null = $state(null);
  let exposedControls: SliderControl[] = $state([]);
  let activeOptionSet: ActiveOptionSet | null = $state(null);
  let designViewport: DesignViewport = $state('desktop');

  // Assistant text can be replayed through upserts while it streams. These
  // keys suppress duplicate projection within a thread and must reset whenever
  // the pane moves to a different design thread.
  let lastClarificationRequestId: string | null = null;
  let lastExposedControlsKey: string | null = null;

  function reset(): void {
    pendingClarification = null;
    exposedControls = [];
    activeOptionSet = null;
    designViewport = 'desktop';
    lastClarificationRequestId = null;
    lastExposedControlsKey = null;
  }

  function applyAssistantPayloadsForItem(item: Item, thread: Thread | null): void {
    if (item.kind !== 'assistant_text') return;
    if (!item.summary) return;
    if (!thread || item.threadId !== thread.id) return;

    const payloads = parseDesignAssistantPayloads(item.summary);
    if (payloads.length === 0) return;
    for (const parsed of payloads) {
      if (parsed.kind === 'clarification_request') {
        if (lastClarificationRequestId === parsed.payload.requestId) continue;
        const next = {
          ...parsed.payload,
          threadId: parsed.payload.threadId || thread.id,
        };
        pendingClarification = next;
        lastClarificationRequestId = next.requestId;
      } else if (parsed.kind === 'expose_controls') {
        const key = controlsKey(parsed.payload.controls);
        if (lastExposedControlsKey === key) continue;
        exposedControls = [...parsed.payload.controls];
        lastExposedControlsKey = key;
      }
    }
  }

  async function applyDesignOptionsUpdate(getCurrentThread: () => Thread | null, threadId: string): Promise<void> {
    if (getCurrentThread()?.id !== threadId) return;
    try {
      const latest = (await LatestDesignOptionSet(threadId)) as
        | { setId: string; optionIds: string[] }
        | null;
      if (getCurrentThread()?.id !== threadId) return;
      if (!latest || !latest.setId || !latest.optionIds || latest.optionIds.length === 0) {
        activeOptionSet = null;
        return;
      }
      const optionPaths = latest.optionIds.map((id) => `options/${latest.setId}/${id}`);
      activeOptionSet = { setId: latest.setId, optionPaths };
    } catch (err) {
      // eslint-disable-next-line no-console
      console.warn('design: LatestDesignOptionSet failed:', err);
    }
  }

  return {
    get pendingClarification() { return pendingClarification; },
    get exposedControls() { return exposedControls; },
    get activeOptionSet() { return activeOptionSet; },
    get designViewport() { return designViewport; },

    reset,
    applyAssistantPayloadsForItem,
    applyDesignOptionsUpdate,

    setPendingClarification(request: ClarificationRequest | null): void {
      pendingClarification = request;
    },

    setExposedControls(controls: SliderControl[]): void {
      exposedControls = [...controls];
    },

    setActiveOptionSet(set: ActiveOptionSet | null): void {
      activeOptionSet = set;
    },

    setDesignViewport(viewport: DesignViewport): void {
      designViewport = viewport;
    },
  };
}

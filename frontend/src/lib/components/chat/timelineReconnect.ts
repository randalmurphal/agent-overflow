import { tick } from 'svelte';
import { onBackendRecovery } from '../../stores/transportRecovery';
import { threadMachine } from '../../stores/attachedBackends.svelte';
import type { ThreadPane } from '../../stores/thread.svelte';
import type { UseStickToBottomController, ReconnectScrollRecovery } from '../../utils/scroll/types';
import type { TimelineVirtualizerHandle } from '../../utils/virtual/types';

/** One mounted timeline's recovery. No state survives a thread switch. */
export function installTimelineReconnect(options: {
  pane: ThreadPane;
  stick: UseStickToBottomController;
  getList?(): TimelineVirtualizerHandle | undefined;
}): () => void {
  const { pane, stick } = options;
  const threadId = pane.threadId;
  let pending: ReconnectScrollRecovery | undefined;
  let frame = 0;
  function cancel(): void {
    pending?.cancel();
    pending = undefined;
    if (frame) cancelAnimationFrame(frame);
    frame = 0;
  }
  const off = onBackendRecovery((backend, phase) => {
    if (!threadId || pane.threadId !== threadId
      || backend !== threadMachine(threadId, pane.thread?.projectId)) return;
    if (phase === 'start') {
      cancel();
      pending = stick.beginReconnectRecovery();
    } else if (phase === 'cancel') {
      cancel();
    } else if (pending) {
      const recovery = pending;
      const failed = (error: unknown): void => {
        if (pending === recovery) cancel();
        console.warn('timelineReconnect: recovery presentation failed', error);
      };
      // Received history is already current. Drain its presentation once,
      // then measure the accumulated layout instead of each network batch.
      try { pane.snapSmoothersToReceived(); }
      catch (error) { failed(error); return; }
      void tick().then(() => {
        if (pending !== recovery) return;
        frame = requestAnimationFrame(() => {
          frame = 0;
          try { options.getList?.()?.revalidate(); }
          catch (error) { failed(error); return; }
          void tick().then(() => {
            if (pending !== recovery) return;
            recovery.finish();
            pending = undefined;
          }).catch(failed);
        });
      }).catch(failed);
    }
  });
  return () => { off(); cancel(); };
}

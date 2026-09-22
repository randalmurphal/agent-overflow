<script lang="ts">
  import type { ThreadPane } from '../../stores/thread.svelte';
  import { trayTaskLabel, trayTaskScopeId, type TrayTask } from '../../utils/backgroundTray';
  import AgentDigestTimeline from '../chat/AgentDigestTimeline.svelte';
  let { pane, task, id }: { pane: ThreadPane; task: TrayTask; id: string } = $props();
  let scopeId = $derived(trayTaskScopeId(task));
</script>

<div data-testid="background-task-tray-row-digest" data-scope-id={scopeId} aria-label="{trayTaskLabel(task)} activity">
  <AgentDigestTimeline {pane} {scopeId} {id} viewKey={`tray:${task.rowId}`}
    live={task.status === 'running'} maxHeight="min(35vh, 14rem)" />
</div>

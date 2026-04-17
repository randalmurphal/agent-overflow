<script lang="ts">
  // DesignView — top-level design-mode container. Mounts the preview panel
  // and artifact list side-by-side, plus the picker when options are pending.
  //
  // Hydrates the artifact history from ListDesignArtifacts on mount or thread
  // switch. Live events take over once the listener is wired in events.ts.

  import type { ThreadPane } from '../../stores/thread.svelte';
  import { ListDesignArtifacts } from '../../stores/bindings';
  import { addToast } from '../../stores/toast.svelte';
  import type { DesignArtifact } from '../../types/design';

  import DesignPreviewPanel from './DesignPreviewPanel.svelte';
  import DesignOptionsPicker from './DesignOptionsPicker.svelte';
  import DesignArtifactList from './DesignArtifactList.svelte';

  let { pane }: { pane: ThreadPane } = $props();

  // Generation guard for hydration fetches so a thread switch mid-flight
  // doesn't overwrite newer state.
  let hydrationGen = 0;

  $effect(() => {
    const threadId = pane.threadId;
    if (!threadId) return;
    const gen = ++hydrationGen;
    ListDesignArtifacts(threadId)
      .then((artifacts: unknown) => {
        if (gen !== hydrationGen) return;
        // Wails returns null rather than an empty array when a thread has
        // no artifacts. Normalise so the rest of the UI can trust the shape.
        const list = Array.isArray(artifacts) ? (artifacts as DesignArtifact[]) : [];
        pane.setDesignArtifacts(list);
      })
      .catch((err: unknown) => {
        if (gen !== hydrationGen) return;
        const message = err instanceof Error ? err.message : String(err);
        addToast('error', `Failed to load design artifacts: ${message}`);
      });
  });
</script>

<div class="flex flex-col h-full min-h-0">
  <div class="flex flex-1 min-h-0">
    <div class="flex-1 min-w-0 flex flex-col">
      <DesignPreviewPanel {pane} />
      {#if pane.pendingDesignOptions}
        <DesignOptionsPicker {pane} />
      {/if}
    </div>
    <DesignArtifactList {pane} />
  </div>
</div>

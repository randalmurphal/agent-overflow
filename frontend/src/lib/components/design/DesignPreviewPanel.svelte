<script lang="ts">
  // DesignPreviewPanel — renders the current design artifact's HTML in a
  // sandboxed iframe with a viewport toggle and an artifact history dropdown.
  //
  // Fetches HTML on demand via GetDesignArtifactHTML so the pane state stays
  // light. Cancels stale fetches if the user switches artifacts mid-flight.

  import type { ThreadPane } from '../../stores/thread.svelte';
  import type { Thread } from '../../types/models';
  import { errString } from '../../utils/errors';
  import {
    CreateThread,
    GetDesignArtifactHTML,
    SaveDraft,
    StartSession,
    UploadAttachment,
  } from '../../stores/bindings';
  import { addToast } from '../../stores/toast.svelte';
  import Button from '../primitives/Button.svelte';
  import { prependThread } from '../../stores/threads.svelte';
  import { captureHtmlToPng, blobToBase64 } from '../../utils/captureHtml';
  import { DESIGN_VIEWPORT_WIDTHS, type DesignViewport } from '../../types/design';

  let { pane }: { pane: ThreadPane } = $props();

  let exporting = $state(false);

  async function exportToNewThread(): Promise<void> {
    if (exporting) return;
    const sourceThread = pane.thread;
    const artifact = activeArtifact;
    if (!sourceThread || !artifact || !fetchedHtml) {
      addToast('warning', 'Nothing to export yet');
      return;
    }

    exporting = true;
    try {
      // 1. Capture the rendered HTML to a PNG Blob (hidden iframe + modern-screenshot).
      const png = await captureHtmlToPng(fetchedHtml, {
        width: DESIGN_VIEWPORT_WIDTHS[pane.designViewport] ?? 1280,
      });

      // 2. Create a sibling thread under the same project/provider.
      // CreateThread moved to a struct-arg signature in Wave 1/2; the
      // export-to-thread flow only needs to forward the source's project
      // context — provider/model/mode default from settings.
      if (!sourceThread.projectId) {
        addToast('error', 'Cannot export: source thread has no project');
        return;
      }
      const newThread = (await CreateThread({
        projectId: sourceThread.projectId,
        provider: sourceThread.provider,
        model: sourceThread.model,
        mode: 'default',
      })) as Thread;

      // 3. Upload the PNG as a draft attachment on the new thread.
      let attachmentId: string | null = null;
      try {
        const base64 = await blobToBase64(png);
        const filename = `design-${artifact.id}.png`;
        const attachment = (await UploadAttachment(
          newThread.id,
          filename,
          'image/png',
          base64,
        )) as { id?: string } | null;
        attachmentId = attachment?.id ?? null;
      } catch (err) {
        console.error('Screenshot upload failed:', err);
        addToast('warning', 'Exported without screenshot — upload failed');
      }

      // 4. Seed the draft with a concise reference + the attachment.
      const prompt = `Design reference: ${artifact.title}\n\nImplement this design.`;
      try {
        await SaveDraft(newThread.id, prompt, attachmentId ? [attachmentId] : [], []);
      } catch (err) {
        console.error('Draft seed failed:', err);
      }

      prependThread(newThread);
      await pane.switchThread(newThread);

      try {
        await StartSession(newThread.id);
      } catch (err) {
        console.error('Failed to start session on exported thread:', err);
      }

      addToast('success', `Exported design to a new thread`);
    } catch (err) {
      console.error('Failed to export design:', err);
      pane.setGeneralError(`Failed to export design: ${errString(err)}`);
    } finally {
      exporting = false;
    }
  }

  // Fetch generation guard — incremented on every fetch kickoff. An in-flight
  // response is applied only if its generation matches the latest value.
  let fetchGeneration = 0;
  let fetchedHtml: string = $state('');
  let fetchError: string | null = $state(null);
  let fetching: boolean = $state(false);

  // Resolve which artifact should be displayed.
  //   - If there are pending options, prefer the first option's artifact so
  //     the iframe previews what's being chosen.
  //   - Otherwise respect an explicit activeArtifactId.
  //   - Otherwise fall back to the latest artifact in history.
  let resolvedArtifactId = $derived.by<string | null>(() => {
    const pending = pane.pendingDesignOptions;
    if (pending && pending.options.length > 0) {
      return pending.options[0].artifactId;
    }
    if (pane.activeArtifactId) return pane.activeArtifactId;
    const history = pane.designArtifacts;
    if (history.length === 0) return null;
    return history[history.length - 1].id;
  });

  let activeArtifact = $derived(
    resolvedArtifactId
      ? pane.designArtifacts.find((a) => a.id === resolvedArtifactId) ?? null
      : null,
  );

  // Fire a fetch whenever the resolved artifact changes.
  $effect(() => {
    const threadId = pane.threadId;
    const artifactId = resolvedArtifactId;
    if (!threadId || !artifactId) {
      fetchedHtml = '';
      fetchError = null;
      fetching = false;
      return;
    }
    const gen = ++fetchGeneration;
    fetching = true;
    fetchError = null;
    GetDesignArtifactHTML(threadId, artifactId)
      .then((html: unknown) => {
        if (gen !== fetchGeneration) return;
        fetchedHtml = typeof html === 'string' ? html : '';
        fetching = false;
      })
      .catch((err: unknown) => {
        if (gen !== fetchGeneration) return;
        fetching = false;
        const message = err instanceof Error ? err.message : String(err);
        fetchError = message;
        addToast('error', `Failed to load design artifact: ${message}`);
      });
  });

  let viewportWidthPx = $derived(DESIGN_VIEWPORT_WIDTHS[pane.designViewport]);

  function selectViewport(next: DesignViewport) {
    pane.setDesignViewport(next);
  }

  function onSelectArtifact(e: Event) {
    const target = e.currentTarget as HTMLSelectElement;
    const value = target.value;
    pane.setActiveArtifact(value === '' ? null : value);
  }
</script>

<div class="flex flex-col h-full min-h-0 bg-transparent">
  <div class="border-b border-border-subtle px-3 py-2 flex items-center gap-2 shrink-0">
    <div class="flex items-center gap-2 min-w-0 flex-1">
      <span class="text-[10px] font-semibold uppercase tracking-wide px-1.5 py-0.5 rounded-[var(--radius-field)] bg-accent/15 text-accent shrink-0">
        Design
      </span>
      {#if pane.designArtifacts.length > 1}
        <select
          aria-label="Select Design Artifact"
          class="text-[12px] bg-surface-0 border border-border-subtle rounded-[var(--radius-field)] px-2 py-1 text-fg max-w-48 truncate focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 transition-colors"
          value={resolvedArtifactId ?? ''}
          onchange={onSelectArtifact}
        >
          {#each pane.designArtifacts as artifact (artifact.id)}
            <option value={artifact.id}>{artifact.title}</option>
          {/each}
        </select>
      {:else if activeArtifact}
        <span class="text-[13px] font-medium text-fg truncate">
          {activeArtifact.title}
        </span>
      {:else}
        <span class="text-[13px] text-fg-muted">Design Preview</span>
      {/if}
      {#if activeArtifact?.description}
        <span class="text-[12px] text-fg-muted truncate" title={activeArtifact.description}>
          — {activeArtifact.description}
        </span>
      {/if}
    </div>
    <div class="flex items-center gap-0.5 shrink-0">
      {#each [['mobile', 'Mobile (375px)', 'M'], ['tablet', 'Tablet (768px)', 'T'], ['desktop', 'Desktop (100%)', 'D']] as [size, label, short] (size)}
        <button
          type="button"
          onclick={() => selectViewport(size as DesignViewport)}
          aria-pressed={pane.designViewport === size}
          aria-label={label}
          title={label}
          class="text-[11px] px-2 py-1 rounded-[var(--radius-field)] cursor-pointer transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40
            {pane.designViewport === size
              ? 'bg-accent text-surface-0'
              : 'text-fg-muted hover:bg-surface-2/30 hover:text-fg'}"
        >
          {short}
        </button>
      {/each}
      {#if activeArtifact}
        <Button
          variant="secondary"
          size="xs"
          onclick={exportToNewThread}
          disabled={!fetchedHtml}
          loading={exporting}
          ariaLabel="Export to New Thread"
          title="Capture a screenshot and open a new thread with this design attached"
          testId="design-export-to-thread"
          class="ml-2"
        >
          {#snippet children()}{exporting ? 'Exporting…' : 'Export →'}{/snippet}
        </Button>
      {/if}
    </div>
  </div>

  <div class="flex-1 min-h-0 overflow-auto bg-surface-0/60 flex items-start justify-center p-2">
    {#if fetchError}
      <div class="text-center text-error text-[13px] p-4">
        <p class="font-medium">Failed to Load Design</p>
        <p class="text-[11px] text-error/80 mt-1">{fetchError}</p>
      </div>
    {:else if !activeArtifact}
      <div class="flex flex-col items-center justify-center h-full text-center text-fg-muted">
        <svg class="w-10 h-10 text-fg-hint mb-3" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
          <circle cx="13.5" cy="6.5" r=".5" fill="currentColor" />
          <circle cx="17.5" cy="10.5" r=".5" fill="currentColor" />
          <circle cx="8.5" cy="7.5" r=".5" fill="currentColor" />
          <circle cx="6.5" cy="12.5" r=".5" fill="currentColor" />
          <path d="M12 2C6.5 2 2 6.5 2 12s4.5 10 10 10c.926 0 1.648-.746 1.648-1.688 0-.437-.18-.835-.437-1.125-.29-.289-.438-.652-.438-1.125a1.64 1.64 0 0 1 1.668-1.668h1.996c3.051 0 5.555-2.503 5.555-5.554C21.965 6.012 17.461 2 12 2z" />
        </svg>
        <p class="text-[13px]">No Design Preview Yet</p>
        <p class="text-[11px] text-fg-hint mt-1">
          Rendered artifacts will appear here when the agent produces a mockup.
        </p>
      </div>
    {:else if fetching && !fetchedHtml}
      <div class="text-[12px] text-fg-muted p-4">Loading preview…</div>
    {:else}
      <iframe
        title={activeArtifact.title}
        srcdoc={fetchedHtml}
        sandbox="allow-scripts"
        referrerpolicy="no-referrer"
        class="h-full rounded-[var(--radius-field)] border border-border-subtle bg-white"
        style="width: {viewportWidthPx ? `${viewportWidthPx}px` : '100%'}; max-width: 100%;"
      ></iframe>
    {/if}
  </div>
</div>

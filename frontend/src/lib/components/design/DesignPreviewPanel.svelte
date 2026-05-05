<script lang="ts">
  // DesignPreviewPanel — the right-hand pane in design threads. Owns the
  // toolbar (viewport selector, refresh, annotate stub, artifact dropdown,
  // send-to-chat menu) and the sandbox iframe that previews the active
  // artifact's HTML. Hydration of the artifact list lives in ChatView so
  // the preview shows the latest render the moment a thread loads.
  //
  // Per the design-mode spec the iframe is `sandbox="allow-scripts"`
  // only — never allow-same-origin — so the artifact HTML cannot escape
  // its document. The PNG capture for "Send to chat" runs through a
  // separate hidden iframe in captureHtml.ts; this iframe stays locked.

  import Smartphone from 'lucide-svelte/icons/smartphone';
  import TabletIcon from 'lucide-svelte/icons/tablet';
  import Monitor from 'lucide-svelte/icons/monitor';
  import RefreshCw from 'lucide-svelte/icons/refresh-cw';
  import MessageSquareText from 'lucide-svelte/icons/message-square-text';
  import MessagesSquare from 'lucide-svelte/icons/messages-square';
  import ImageIcon from 'lucide-svelte/icons/image';
  import FileText from 'lucide-svelte/icons/file-text';
  import Layers from 'lucide-svelte/icons/layers';
  import ChevronDown from 'lucide-svelte/icons/chevron-down';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import type { Thread } from '../../types/models';
  import { errString } from '../../utils/errors';
  import { relativeTime } from '../../utils/format';
  import {
    CreateThread,
    GetDesignArtifactHTML,
    GetDesignArtifactPng,
    SaveDraft,
    StartSession,
    UploadAttachment,
  } from '../../stores/bindings';
  import { addToast } from '../../stores/toast.svelte';
  import { prependThread } from '../../stores/threads.svelte';
  import { captureHtmlToPng, blobToBase64 } from '../../utils/captureHtml';
  import { DESIGN_VIEWPORT_WIDTHS, type DesignViewport } from '../../types/design';
  import Icon from '../primitives/Icon.svelte';
  import Menu from '../primitives/Menu.svelte';
  import MenuItem from '../primitives/MenuItem.svelte';
  import Popover from '../primitives/Popover.svelte';

  let { pane }: { pane: ThreadPane } = $props();

  // -- Artifact resolution & HTML fetch ---------------------------------

  // Fetch generation guard — incremented on every fetch kickoff. An in-flight
  // response is applied only if its generation matches the latest value.
  let fetchGeneration = 0;
  let fetchedHtml: string = $state('');
  let fetchError: string | null = $state(null);
  let fetching: boolean = $state(false);

  // Resolve which artifact should be displayed.
  //   - If there are pending options, prefer the first option's artifact
  //     so the iframe previews what's being chosen.
  //   - Otherwise respect an explicit activeArtifactId (the dropdown).
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
    void fetchHtml(threadId, artifactId);
  });

  async function fetchHtml(threadId: string, artifactId: string): Promise<void> {
    const gen = ++fetchGeneration;
    fetching = true;
    fetchError = null;
    try {
      const html = (await GetDesignArtifactHTML(threadId, artifactId)) as unknown;
      if (gen !== fetchGeneration) return;
      fetchedHtml = typeof html === 'string' ? html : '';
      fetching = false;
    } catch (err) {
      if (gen !== fetchGeneration) return;
      fetching = false;
      const message = err instanceof Error ? err.message : String(err);
      fetchError = message;
      addToast('error', `Failed to load design artifact: ${message}`);
    }
  }

  function refresh(): void {
    const threadId = pane.threadId;
    const artifactId = resolvedArtifactId;
    if (!threadId || !artifactId) return;
    void fetchHtml(threadId, artifactId);
  }

  // -- Viewport selector -----------------------------------------------

  const VIEWPORTS: ReadonlyArray<{
    value: DesignViewport;
    label: string;
    icon: typeof Smartphone;
  }> = [
    { value: 'mobile', label: 'Mobile', icon: Smartphone },
    { value: 'tablet', label: 'Tablet', icon: TabletIcon },
    { value: 'desktop', label: 'Desktop', icon: Monitor },
  ];

  let viewportWidthPx = $derived(DESIGN_VIEWPORT_WIDTHS[pane.designViewport]);

  function selectViewport(next: DesignViewport) {
    pane.setDesignViewport(next);
  }

  // -- Artifact dropdown -----------------------------------------------

  // History is stored newest-last, but the dropdown lists newest-first
  // and labels each entry with its v{n} version (newest = highest n).
  // Re-deriving from pane.designArtifacts keeps the labels in sync as
  // new artifacts stream in from design:artifact events.
  type ArtifactRow = {
    id: string;
    title: string;
    createdAt: number;
    version: number;
  };
  let artifactRows = $derived<ArtifactRow[]>(
    pane.designArtifacts
      .map((a, i) => ({
        id: a.id,
        title: a.title,
        createdAt: a.createdAt,
        version: i + 1,
      }))
      .reverse(),
  );

  let activeRow = $derived<ArtifactRow | null>(
    resolvedArtifactId
      ? artifactRows.find((r) => r.id === resolvedArtifactId) ?? null
      : null,
  );

  let dropdownTriggerEl: HTMLElement | undefined = $state(undefined);
  let dropdownOpen = $state(false);

  function dropdownLabel(row: ArtifactRow | null): string {
    if (!row) return 'No render yet';
    return `v${row.version} · ${relativeTime(row.createdAt)} · ${row.title}`;
  }

  function selectArtifact(id: string) {
    pane.setActiveArtifact(id);
    dropdownOpen = false;
  }

  // -- Send to chat menu ------------------------------------------------

  type HandoffShape = 'bundle' | 'html-summary' | 'png-only';

  let sendMenuTriggerEl: HTMLElement | undefined = $state(undefined);
  let sendMenuOpen = $state(false);
  let exporting = $state(false);

  async function handoff(shape: HandoffShape): Promise<void> {
    sendMenuOpen = false;
    if (exporting) return;
    const sourceThread = pane.thread;
    const artifact = activeArtifact;
    if (!sourceThread || !artifact || !fetchedHtml) {
      addToast('warning', 'Nothing to export yet');
      return;
    }
    if (!sourceThread.projectId) {
      addToast('error', 'Cannot export: source thread has no project');
      return;
    }

    exporting = true;
    try {
      // 1. Resolve the PNG: prefer a pre-captured one (saved at render
      //    time by the design:artifact handler) and fall back to a
      //    fresh capture if the persisted file is missing or the shape
      //    being exported needs a desktop-sized snapshot we don't have.
      let pngBase64: string | null = null;
      if (shape !== 'html-summary') {
        try {
          const persisted = (await GetDesignArtifactPng(
            sourceThread.id,
            artifact.id,
          )) as string;
          if (persisted) pngBase64 = persisted;
        } catch (err) {
          console.warn('GetDesignArtifactPng failed; will re-capture:', err);
        }
        if (pngBase64 === null) {
          try {
            const png = await captureHtmlToPng(fetchedHtml, {
              width: DESIGN_VIEWPORT_WIDTHS[pane.designViewport] ?? 1280,
            });
            pngBase64 = await blobToBase64(png);
          } catch (err) {
            console.error('PNG capture during export failed:', err);
            if (shape === 'png-only') {
              addToast('error', 'Could not capture PNG');
              return;
            }
            // Bundle shape can degrade to "HTML + summary" if capture fails.
            addToast('warning', 'PNG capture failed; exporting HTML only');
            shape = 'html-summary';
          }
        }
      }

      // 2. Create a sibling chat thread under the same project/provider.
      const newThread = (await CreateThread({
        projectId: sourceThread.projectId,
        provider: sourceThread.provider,
        model: sourceThread.model,
        mode: 'default',
      })) as Thread;

      // 3. Upload the PNG (when we have one) as a draft attachment.
      let attachmentId: string | null = null;
      if (pngBase64 && shape !== 'html-summary') {
        try {
          const filename = `design-${artifact.id}.png`;
          const attachment = (await UploadAttachment(
            newThread.id,
            filename,
            'image/png',
            pngBase64,
          )) as { id?: string } | null;
          attachmentId = attachment?.id ?? null;
        } catch (err) {
          console.error('Screenshot upload failed:', err);
          if (shape === 'png-only') {
            addToast('warning', 'Exported without screenshot — upload failed');
          }
        }
      }

      // 4. Compose the seed message body. Each shape has a different
      //    text spine; the attachment carries the visual.
      const prompt = composeHandoffPrompt(shape, artifact.title, fetchedHtml);
      try {
        await SaveDraft(
          newThread.id,
          prompt,
          attachmentId ? [attachmentId] : [],
          [],
          null,
        );
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

      addToast('success', `Sent ${labelForShape(shape)} to a new thread`);
    } catch (err) {
      console.error('Failed to export design:', err);
      pane.setGeneralError(`Failed to export design: ${errString(err)}`);
    } finally {
      exporting = false;
    }
  }

  function labelForShape(shape: HandoffShape): string {
    switch (shape) {
      case 'bundle':
        return 'design bundle';
      case 'html-summary':
        return 'HTML + summary';
      case 'png-only':
        return 'PNG render';
    }
  }

  function composeHandoffPrompt(
    shape: HandoffShape,
    title: string,
    html: string,
  ): string {
    const header = `Design reference: ${title}\n\nImplement this design.`;
    if (shape === 'png-only') return header;
    if (shape === 'html-summary' || shape === 'bundle') {
      return `${header}\n\n<details>\n<summary>Source HTML</summary>\n\n\`\`\`html\n${html}\n\`\`\`\n\n</details>`;
    }
    return header;
  }
</script>

<div class="flex flex-col h-full min-h-0 bg-transparent">
  <!-- Toolbar — left cluster: viewport · refresh · annotate · dropdown.
       Right cluster: send-to-chat menu. -->
  <div class="flex items-center gap-2 border-b border-border-subtle px-3 py-2 shrink-0 min-w-0">
    <div class="flex items-center gap-0.5 shrink-0">
      {#each VIEWPORTS as vp (vp.value)}
        <button
          type="button"
          onclick={() => selectViewport(vp.value)}
          aria-pressed={pane.designViewport === vp.value}
          aria-label={vp.label}
          title={vp.label}
          class={[
            'inline-flex items-center justify-center rounded-[var(--radius-field)]',
            'p-1 cursor-pointer transition-colors',
            'focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40',
            pane.designViewport === vp.value
              ? 'bg-accent/15 text-fg'
              : 'text-fg-muted hover:bg-surface-2/30 hover:text-fg',
          ].join(' ')}
        >
          <Icon icon={vp.icon} size={14} strokeWidth={1.6} />
        </button>
      {/each}
    </div>

    <div class="h-4 w-px bg-border-subtle/60 shrink-0" aria-hidden="true"></div>

    <button
      type="button"
      onclick={refresh}
      disabled={!resolvedArtifactId || fetching}
      aria-label="Refresh Preview"
      title="Refresh Preview"
      class={[
        'inline-flex items-center justify-center rounded-[var(--radius-field)]',
        'p-1 text-fg-muted cursor-pointer transition-colors',
        'hover:text-fg hover:bg-surface-2/30',
        'focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40',
        'disabled:opacity-50 disabled:cursor-not-allowed',
      ].join(' ')}
      data-testid="design-refresh"
    >
      <Icon
        icon={RefreshCw}
        size={13}
        strokeWidth={1.7}
        class={fetching ? 'animate-spin' : ''}
      />
    </button>

    <!-- Annotate / comment-mode toggle. The interaction model is
         specified in docs/architecture/design-mode.md but the feature
         is deferred — render the affordance disabled with a "coming
         soon" tooltip so the layout matches the final shape. -->
    <button
      type="button"
      disabled
      aria-label="Annotate (coming soon)"
      title="Annotate — coming soon"
      class={[
        'inline-flex items-center justify-center rounded-[var(--radius-field)]',
        'p-1 text-fg-hint',
        'cursor-not-allowed',
      ].join(' ')}
      data-testid="design-annotate"
    >
      <Icon icon={MessageSquareText} size={13} strokeWidth={1.6} />
    </button>

    <!-- Artifact dropdown. Native <select> can't show the version + time
         + title triplet cleanly, so this is a Popover-anchored Menu. -->
    <span bind:this={dropdownTriggerEl} class="inline-block min-w-0 max-w-[280px]">
      <button
        type="button"
        onclick={() => (dropdownOpen = !dropdownOpen)}
        disabled={artifactRows.length === 0}
        aria-haspopup="menu"
        aria-expanded={dropdownOpen}
        title={dropdownLabel(activeRow)}
        class={[
          'inline-flex items-center gap-1 rounded-[var(--radius-field)]',
          'border border-border-subtle bg-surface-0 px-2 py-1',
          'text-[12px] text-fg max-w-full',
          'cursor-pointer transition-colors',
          'hover:border-border focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40',
          'disabled:opacity-60 disabled:cursor-not-allowed',
        ].join(' ')}
        data-testid="design-artifact-dropdown"
      >
        <span class="truncate">{dropdownLabel(activeRow)}</span>
        <Icon icon={ChevronDown} size={12} strokeWidth={1.6} class="opacity-70 shrink-0" />
      </button>
    </span>
    <Popover
      anchor={dropdownTriggerEl}
      open={dropdownOpen}
      onClose={() => (dropdownOpen = false)}
      placement="bottom-start"
      role="menu"
      ariaLabel="Design Artifact History"
    >
      {#snippet children()}
        <Menu ariaLabel="Design Artifact History" onClose={() => (dropdownOpen = false)}>
          {#snippet children()}
            {#each artifactRows as row (row.id)}
              <MenuItem
                label={dropdownLabel(row)}
                checked={resolvedArtifactId === row.id}
                onSelect={() => selectArtifact(row.id)}
              />
            {/each}
          {/snippet}
        </Menu>
      {/snippet}
    </Popover>

    <!-- Right cluster: send-to-chat menu. -->
    <div class="ml-auto flex items-center gap-1 shrink-0">
      <span bind:this={sendMenuTriggerEl}>
        <button
          type="button"
          onclick={() => (sendMenuOpen = !sendMenuOpen)}
          disabled={exporting || !activeArtifact || !fetchedHtml}
          aria-haspopup="menu"
          aria-expanded={sendMenuOpen}
          title="Send to chat…"
          class={[
            'inline-flex items-center gap-1 rounded-[var(--radius-field)]',
            'border border-border-subtle bg-surface-0 px-2 py-1',
            'text-[12px] text-fg cursor-pointer transition-colors',
            'hover:border-border focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40',
            'disabled:opacity-60 disabled:cursor-not-allowed',
          ].join(' ')}
          data-testid="design-send-to-chat"
        >
          <span>{exporting ? 'Exporting…' : 'Send to chat…'}</span>
          <Icon icon={ChevronDown} size={12} strokeWidth={1.6} class="opacity-70" />
        </button>
      </span>
      <Popover
        anchor={sendMenuTriggerEl}
        open={sendMenuOpen}
        onClose={() => (sendMenuOpen = false)}
        placement="bottom-end"
        role="menu"
        ariaLabel="Send to Chat"
      >
        {#snippet children()}
          <Menu ariaLabel="Send to Chat" onClose={() => (sendMenuOpen = false)}>
            {#snippet children()}
              <MenuItem
                label="Bundle (HTML + summary + PNG)"
                description="Recommended — multimodal models read both"
                onSelect={() => void handoff('bundle')}
              >
                {#snippet icon()}
                  <Icon icon={Layers} size={13} strokeWidth={1.6} />
                {/snippet}
              </MenuItem>
              <MenuItem
                label="HTML + summary"
                onSelect={() => void handoff('html-summary')}
              >
                {#snippet icon()}
                  <Icon icon={FileText} size={13} strokeWidth={1.6} />
                {/snippet}
              </MenuItem>
              <MenuItem
                label="PNG render only"
                onSelect={() => void handoff('png-only')}
              >
                {#snippet icon()}
                  <Icon icon={ImageIcon} size={13} strokeWidth={1.6} />
                {/snippet}
              </MenuItem>
            {/snippet}
          </Menu>
        {/snippet}
      </Popover>
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
        <Icon icon={MessagesSquare} size={36} strokeWidth={1.2} class="text-fg-hint mb-3" />
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

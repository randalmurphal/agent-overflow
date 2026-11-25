<script module lang="ts">
  import { createCachedComponentLoader, type CachedComponentLoader } from '../../utils/lazyComponent';
  import type { CompanionPanelKind } from '../../stores/companionPanes.svelte';

  // Panel bodies load lazily so browser/plan/review feature chunks stay out of
  // the eager startup graph, and cache their resolved component at module scope
  // so warm reopens and thread switches mount synchronously on frame 1.
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  const COMPANION_LOADERS: Record<CompanionPanelKind, CachedComponentLoader<any>> = {
    plan: createCachedComponentLoader(() => import('../chat/PlanSidebar.svelte')),
    review: createCachedComponentLoader(() => import('../review/ReviewPane.svelte')),
    agent: createCachedComponentLoader(() => import('../agent/AgentPane.svelte')),
    browser: createCachedComponentLoader(() => import('../browser/BrowserPane.svelte')),
  };
</script>

<script lang="ts">
  import {
    closeCompanion,
  } from '../../stores/companionPanes.svelte';
  import { makePanelContext } from '../../stores/panelContext.svelte';
  import { getPane } from '../../stores/panes.svelte';
  import { companionSubjectKey } from '../../stores/companionSubject';

  // take-control and side-chat are companions in the registry but not panel
  // bodies: each renders its own whole surface (the PTY mirror, a ChatView
  // over the side chat's own thread) through PaneHost's dedicated branches,
  // so neither arrives here.
  interface Props {
    paneId: string;
    kind: CompanionPanelKind;
    sourcePaneId: string;
  }

  let { paneId, kind, sourcePaneId }: Props = $props();
  // Captured ONCE, deliberately non-reactive: a companion's kind is fixed for
  // its lifetime, and divider drags replace the layout item behind `kind` every
  // frame. Thread switches remount the body through {#key panelKey}.
  // svelte-ignore state_referenced_locally
  const loader = COMPANION_LOADERS[kind];
  const cachedComponent = loader.peek();
  const panelLoad = cachedComponent ? null : loader.load();
  let sourcePane = $derived(getPane(sourcePaneId));
  let panelContext = $derived(
    sourcePane ? makePanelContext(sourcePane, () => closeCompanion(paneId)) : null,
  );
  let panelKey = $derived(
    sourcePane?.thread && panelContext ? `${companionSubjectKey(sourcePane)}:${kind}` : '',
  );
</script>

{#if sourcePane && panelContext}
  <aside
    aria-label={kind === 'plan'
      ? 'Proposed Plan'
      : kind === 'review'
        ? 'Review'
        : kind === 'agent'
          ? 'Agent'
          : 'Browser'}
    class="flex h-full min-h-0 flex-col border-l border-border bg-surface-1"
    data-testid={`companion-pane-${kind}`}
    data-companion-pane-id={paneId}
    data-companion-source-pane-id={sourcePaneId}
  >
    {#key panelKey}
      {#if cachedComponent}
        {@const PanelComponent = cachedComponent}
        <PanelComponent ctx={panelContext} />
      {:else if panelLoad}
        {#await panelLoad then PanelComponent}
          <PanelComponent ctx={panelContext} />
        {:catch err}
          <div class="flex h-full min-h-0 items-center justify-center px-4 text-sm text-error" data-testid="companion-pane-load-error">
            Failed to load panel: {err instanceof Error ? err.message : String(err)}
          </div>
        {/await}
      {/if}
    {/key}
  </aside>
{:else}
  <div
    class="flex h-full min-h-0 items-center justify-center px-4 text-sm text-error"
    data-testid="companion-pane-broken"
    data-companion-pane-id={paneId}
    data-companion-source-pane-id={sourcePaneId}
  >
    Companion pane unavailable.
  </div>
{/if}

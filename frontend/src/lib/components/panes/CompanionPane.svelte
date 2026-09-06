<script lang="ts">
  import type { Component } from 'svelte';
  import {
    closeCompanion,
    type CompanionPanelKind,
  } from '../../stores/companionPanes.svelte';
  import { makePanelContext } from '../../stores/panelContext.svelte';
  import { getPane } from '../../stores/panes.svelte';
  import { companionSubjectKey } from '../../stores/companionSubject';

  // take-control is a companion in the registry but not a panel body — it
  // renders its own surface (TakeControlPane) through PaneHost's dedicated
  // branch, so it never arrives here.
  interface Props {
    paneId: string;
    kind: CompanionPanelKind;
    sourcePaneId: string;
  }

  // Panel bodies load lazily so browser/plan/review feature chunks
  // stay out of the eager startup graph — a companion pane only exists
  // after an explicit user action.
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  type CompanionLoader = () => Promise<{ default: Component<any> }>;

  const COMPANION_LOADERS = {
    plan: () => import('../chat/PlanSidebar.svelte'),
    review: () => import('../review/ReviewPane.svelte'),
    agent: () => import('../agent/AgentPane.svelte'),
    browser: () => import('../browser/BrowserPane.svelte'),
  } satisfies Record<CompanionPanelKind, CompanionLoader>;

  let { paneId, kind, sourcePaneId }: Props = $props();
  // Captured ONCE, deliberately non-reactive: an {#await} block re-runs
  // its expression whenever any dependency invalidates, with no
  // promise-identity cutoff — and a divider drag replaces this pane's
  // layout item (the source behind the `kind` prop) every frame. Calling
  // the loader inside the template minted a fresh promise per frame,
  // flashing the pending branch and remounting the panel body mid-drag
  // (full review reload + scroll reset). A companion's kind is fixed for
  // its lifetime, so one promise is correct; thread switches remount the
  // body through {#key panelKey} without changing kind.
  // svelte-ignore state_referenced_locally
  const panelLoad = COMPANION_LOADERS[kind]();
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
      {#await panelLoad then { default: PanelComponent }}
        <PanelComponent ctx={panelContext} />
      {:catch err}
        <div class="flex h-full min-h-0 items-center justify-center px-4 text-sm text-error" data-testid="companion-pane-load-error">
          Failed to load panel: {err instanceof Error ? err.message : String(err)}
        </div>
      {/await}
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

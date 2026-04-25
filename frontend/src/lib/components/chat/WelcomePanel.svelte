<script lang="ts">
  // First-launch welcome panel + chat-empty surface.
  //
  // Replaces the older passive "Select or create a thread" placeholder.
  // Its job is to give the user a clear next action whether they're
  // brand new (no projects) or returning (projects exist but no thread
  // selected) and to surface provider-binary install hints up-front so
  // the user discovers a missing CLI before their first send fails.
  //
  // The panel deliberately doesn't own the AddProjectModal or the
  // create-thread flow — both live in ProjectsSection. We dispatch
  // CustomEvents and let the sidebar do the work, which keeps this
  // component a pure projection.

  import type { ThreadPane } from '../../stores/thread.svelte';
  import { getProjects } from '../../stores/projects.svelte';
  import { getProviderStatus } from '../../stores/providerStatus.svelte';
  import { openCheatSheet } from '../../stores/cheatSheet.svelte';
  import { GetProviderStatuses } from '../../stores/bindings';
  import Button from '../primitives/Button.svelte';
  import ProviderPill from '../shared/ProviderPill.svelte';

  // Pane is reserved for future state (e.g. loading flag while a draft
  // restore is in flight) but unused today; kept on the prop surface so
  // the embedding contract stays the same as the rest of the chat
  // surfaces.
  // eslint-disable-next-line @typescript-eslint/no-unused-vars
  let { pane: _pane }: { pane: ThreadPane } = $props();

  let hasProjects = $derived(getProjects().length > 0);

  const providers = ['claude', 'codex'] as const;

  // Local snapshot of provider statuses keyed by provider name. Filled
  // by the on-mount probe below and kept in sync with subsequent
  // `provider:status` events via the store. Local state means we don't
  // care whether the boot-time probe events arrived before this panel
  // mounted (they often don't — the probe runs on a goroutine before
  // the SPA's wsClient subscribes).
  type Snapshot = { status: string; actionUrl?: string };
  let snapshots = $state<Record<string, Snapshot>>({});

  // Re-read the store every render so subsequent live events update the
  // pill — the on-mount fetch covers the boot-race; `recordProviderStatus`
  // updates the store later (e.g. a manual recheck) and this $derived
  // picks the freshest value across both sources.
  let claudeInfo: Snapshot = $derived.by(() => {
    const evt = getProviderStatus('claude');
    if (evt) return { status: evt.status ?? 'unknown', actionUrl: evt.actionUrl };
    return snapshots.claude ?? { status: 'unknown' };
  });
  let codexInfo: Snapshot = $derived.by(() => {
    const evt = getProviderStatus('codex');
    if (evt) return { status: evt.status ?? 'unknown', actionUrl: evt.actionUrl };
    return snapshots.codex ?? { status: 'unknown' };
  });

  function infoFor(name: 'claude' | 'codex'): Snapshot {
    return name === 'claude' ? claudeInfo : codexInfo;
  }

  // On mount, fetch fresh provider statuses. GetProviderStatuses re-runs
  // detection (cheap — already cached on the Go side) and re-emits
  // events, so the providerStatus store also stays current. We hold a
  // local snapshot as fallback for the boot-race window where events
  // fired before this panel mounted.
  $effect(() => {
    let cancelled = false;
    GetProviderStatuses()
      .then((statuses) => {
        if (cancelled) return;
        const next: Record<string, Snapshot> = {};
        for (const ps of statuses) {
          if (!ps.provider) continue;
          next[ps.provider] = {
            status: ps.status ?? (ps.installed ? 'ready' : 'not_found'),
          };
        }
        snapshots = next;
      })
      .catch(() => {
        // Network / RPC error during the welcome panel's lifetime is
        // rare and recovers via the connection-status banner. Leave
        // the pills on "unknown" rather than misreporting.
      });
    return () => {
      cancelled = true;
    };
  });

  function handleAddProject(): void {
    window.dispatchEvent(new CustomEvent('agent-overflow:open-add-project'));
  }

  function handleNewThread(): void {
    window.dispatchEvent(new CustomEvent('agent-overflow:new-thread-in-active-project'));
  }

  function handleCheatSheet(): void {
    openCheatSheet();
  }
</script>

<div
  class="flex h-full w-full flex-col items-center justify-center gap-6 px-8"
  data-testid="welcome-panel"
>
  <h1 class="text-2xl font-semibold text-fg">Welcome to Agent Overflow</h1>
  <p class="max-w-md text-center text-sm text-fg-muted">
    Pair Claude Code or Codex with a polished UI for managing threads, diffs, and approvals.
  </p>

  <div class="flex gap-2" data-testid="welcome-provider-pills">
    {#each providers as provider}
      {@const info = infoFor(provider)}
      <ProviderPill {provider} status={info.status} actionUrl={info.actionUrl} />
    {/each}
  </div>

  <div class="flex gap-3" data-testid="welcome-ctas">
    {#if hasProjects}
      <Button variant="primary" size="md" onclick={handleNewThread} testId="welcome-new-thread">
        {#snippet children()}New Thread{/snippet}
      </Button>
    {:else}
      <Button variant="primary" size="md" onclick={handleAddProject} testId="welcome-add-project">
        {#snippet children()}Add a Project{/snippet}
      </Button>
    {/if}
    <Button variant="secondary" size="md" onclick={handleCheatSheet} testId="welcome-cheat-sheet">
      {#snippet children()}Keyboard Shortcuts{/snippet}
    </Button>
  </div>
</div>

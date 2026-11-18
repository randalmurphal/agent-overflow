<script lang="ts">
  // An assistant that runs with two memories (docs/specs/standing-agents.md,
  // "Memory"): its computer's setting for its provider's own memory is
  // "never" and that provider's own memory is on, so the assistant may save
  // what it learns there instead of in Agent Overflow memory. Not a session
  // claim and not dismissible: it shows while both facts hold, and its one
  // action opens the memory settings. The setting is live in the settings
  // store; the provider's own memory is configured outside the app, so it is
  // read when the banner could show and again each time the window comes
  // back. Shown on an assistant's current thread only, never on its workers
  // or an ordinary thread.
  import { fade } from 'svelte/transition';
  import { GetNativeMemoryStatus } from '../../stores/bindings';
  import { threadMachine } from '../../stores/attachedBackends.svelte';
  import { computerHasSetting, getSettings } from '../../stores/settings.svelte';
  import { openSettingsOverlay } from '../../stores/settingsOverlay.svelte';
  import { withBackendTarget } from '../../transport/backends';
  import { hasScope } from '../../transport/scopes';
  import { reportFrontendDiagnostic } from '../../utils/frontendErrorCapture';
  import { errString } from '../../utils/errors';
  import type { Thread } from '../../types/models';

  let { thread }: { thread: Thread | null } = $props();

  let backend = $derived(thread ? threadMachine(thread.id, thread.projectId) : null);
  // claude-tui runs the same Claude Code, with the same memory.
  let codex = $derived(thread?.provider === 'codex');
  let key = $derived(codex ? 'codexOwnMemoryOff' as const : 'claudeOwnMemoryOff' as const);
  let providerName = $derived(codex ? 'Codex' : 'Claude');
  let never = $derived(
    !!thread?.agentId && backend !== null && computerHasSetting(key, backend) && getSettings(backend)[key] === 'never',
  );

  let ownMemoryOn = $state(false);
  // Only the newest read's answer lands, and none once the banner cannot
  // show or the component is gone.
  let read = 0;

  async function readOwnMemory(target: NonNullable<typeof backend>, codexThread: boolean): Promise<void> {
    const mine = ++read;
    try {
      const status = await withBackendTarget(target, () => GetNativeMemoryStatus());
      // A computer whose release has no such read answers none.
      if (mine === read) ownMemoryOn = (codexThread ? status?.codex : status?.claude) ?? false;
    } catch (err) {
      if (mine !== read) return;
      ownMemoryOn = false;
      reportFrontendDiagnostic('Could not read whether a provider\'s own memory is on for an assistant banner', errString(err));
    }
  }

  $effect(() => {
    const target = backend;
    const codexThread = codex;
    if (!never || target === null || !hasScope('settings:read', target)) {
      read++;
      ownMemoryOn = false;
      return;
    }
    void readOwnMemory(target, codexThread);
    const refresh = (): void => {
      if (document.visibilityState === 'visible') void readOwnMemory(target, codexThread);
    };
    window.addEventListener('focus', refresh);
    document.addEventListener('visibilitychange', refresh);
    return () => {
      read++;
      window.removeEventListener('focus', refresh);
      document.removeEventListener('visibilitychange', refresh);
    };
  });

  let shown = $derived(never && ownMemoryOn);
</script>

{#if shown && backend !== null}
  <div
    transition:fade={{ duration: 150 }}
    role="alert"
    aria-live="polite"
    data-testid="own-memory-banner"
    class="border-b bg-warning/15 border-warning/30 text-warning px-4 py-2 flex items-center gap-2"
  >
    <p class="text-xs flex-1 line-clamp-2">
      {providerName}'s own memory is also on for this assistant, so it may save what it learns there instead of in
      Agent Overflow memory.
    </p>
    <button
      onclick={() => openSettingsOverlay('memory', backend ?? undefined)}
      data-testid="own-memory-banner-settings"
      class="text-xs px-2 py-0.5 compact:px-3 compact:py-2 rounded border border-current/30 hover:bg-fg/10 cursor-pointer shrink-0 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
    >
      Memory settings
    </button>
  </div>
{/if}

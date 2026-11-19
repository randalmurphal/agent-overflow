<script lang="ts">
  // An assistant that runs with two memories (docs/specs/standing-agents.md,
  // "Two memories"): its computer's setting for its provider's own memory is
  // "never" and that provider's own memory is on, so the assistant may save
  // what it learns there instead of in Agent Overflow memory. Not a session
  // claim and not dismissible: it shows while both facts hold, and its one
  // action opens the memory settings. The setting is live in the settings
  // store; the provider's own memory is that computer's answer in
  // providerMemory.svelte.ts, watched while the banner could show. A
  // provider whose configuration cannot be read shows no banner; Settings >
  // Memory says why. Shown on an assistant's current thread only, never on
  // its workers or an ordinary thread. ChatView gives it a row of its own
  // above the conversation.
  import { threadMachine } from '../../stores/attachedBackends.svelte';
  import { providerMemory, watchProviderMemory } from '../../stores/providerMemory.svelte';
  import { computerHasSetting, getSettings } from '../../stores/settings.svelte';
  import { openSettingsOverlay } from '../../stores/settingsOverlay.svelte';
  import { hasScope } from '../../transport/scopes';
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

  $effect(() => {
    if (!never || backend === null || !hasScope('settings:read', backend)) return;
    return watchProviderMemory(backend);
  });

  let shown = $derived.by(() => {
    if (!never || backend === null) return false;
    const status = providerMemory(backend)?.status;
    const own = codex ? status?.codex : status?.claude;
    return !!own?.on;
  });
</script>

{#if shown && backend !== null}
  <div
    role="alert"
    aria-live="polite"
    data-testid="own-memory-banner"
    class="shrink-0 border-b bg-warning/15 border-warning/30 text-warning px-4 py-2 flex items-center gap-2"
  >
    <p class="text-xs flex-1 min-w-0">
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

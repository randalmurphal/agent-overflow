<script lang="ts">
  // Status pill for a single provider on the welcome panel. Shows a
  // green check + "Claude ready" / "Codex ready" when the binary
  // resolved and the version is acceptable; otherwise shows a red x +
  // a CTA pointing at the install / docs URL. The ready / not-ready
  // discrimination uses the same string literals the wire pushes.
  //
  // Status values are kept loose because the welcome panel might mount
  // before the startup probe finishes — `'unknown'` renders a neutral
  // dash so the user doesn't see a flash of red while detection is in
  // flight.
  //
  // Action URL comes from the provider:status event payload (Go-side
  // table in app_provider_status.go); we intentionally don't harden it
  // to a specific URL here so future provider additions or doc moves
  // don't require touching the frontend.

  import CheckCircle2 from 'lucide-svelte/icons/check-circle-2';
  import XCircle from 'lucide-svelte/icons/x-circle';
  import HelpCircle from 'lucide-svelte/icons/help-circle';
  import Icon from '../primitives/Icon.svelte';

  type ProviderName = 'claude' | 'codex';

  interface Props {
    provider: ProviderName;
    status: string;
    actionUrl?: string;
  }

  let { provider, status, actionUrl }: Props = $props();

  const PROVIDER_LABEL: Record<ProviderName, string> = {
    claude: 'Claude',
    codex: 'Codex',
  };

  let label = $derived(PROVIDER_LABEL[provider]);

  let ready = $derived(status === 'ready');
  let unknown = $derived(status === 'unknown' || status === '');
  let pillClasses = $derived.by(() => {
    if (ready) return 'bg-success/10 border-success/30 text-success';
    if (unknown) return 'bg-surface-1 border-border-subtle text-fg-muted';
    return 'bg-error/10 border-error/30 text-error';
  });

  let statusText = $derived.by(() => {
    if (ready) return `${label} ready`;
    if (unknown) return `Checking ${label}…`;
    if (status === 'unauthenticated') return `${label} not signed in`;
    if (status === 'version_too_old') return `${label} version too old`;
    return `${label} not installed`;
  });

  function handleAction(): void {
    if (!actionUrl) return;
    window.open(actionUrl, '_blank', 'noopener,noreferrer');
  }
</script>

<span
  class="inline-flex items-center gap-1.5 rounded-full border px-2.5 py-1 text-xs {pillClasses}"
  data-testid="provider-pill"
  data-provider={provider}
  data-status={status || 'unknown'}
>
  {#if ready}
    <Icon icon={CheckCircle2} size={12} strokeWidth={2.2} />
  {:else if unknown}
    <Icon icon={HelpCircle} size={12} strokeWidth={2.2} />
  {:else}
    <Icon icon={XCircle} size={12} strokeWidth={2.2} />
  {/if}
  <span>{statusText}</span>
  {#if !ready && !unknown && actionUrl}
    <button
      type="button"
      onclick={handleAction}
      class="ml-1 underline underline-offset-2 hover:opacity-80 cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50 rounded"
      data-testid="provider-pill-install"
    >
      Install
    </button>
  {/if}
</span>

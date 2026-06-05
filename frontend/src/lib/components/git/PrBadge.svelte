<script lang="ts">
  // Branch PR/MR badge for the chat header. Rendered only when the workspace
  // branch has an open pull/merge request (status.openPrUrl). The label uses
  // the forge-correct noun + sigil (PR #123 on GitHub, MR !123 on GitLab).
  //
  // It is a real <a href> — no onclick. The app-wide external-link delegate
  // (utils/externalLinks, installed once in App.svelte) intercepts the click
  // and routes it through window.open (remote / --connect) or the
  // OpenExternalURL host binding (loopback / native, incl. WSL → Windows), so
  // the link behaves like every other external link in the app and supports
  // middle-click. safeExternalURL gates rendering, mirroring the delegate's own
  // scheme/host validation. Presentational: status is passed in.
  import type { GitStatus } from '../../types/git';
  import { forgeLabels } from '../../utils/forgeLabels';
  import { safeExternalURL } from '../../utils/externalLinks';

  interface Props {
    status: GitStatus | null;
  }

  let { status }: Props = $props();

  let href = $derived(safeExternalURL(status?.openPrUrl));
  let labels = $derived(forgeLabels(status?.forge));
  let label = $derived(
    status?.openPrNumber
      ? `${labels.noun} ${labels.numberSigil}${status.openPrNumber}`
      : labels.noun,
  );
</script>

{#if href}
  <a
    {href}
    target="_blank"
    rel="noopener noreferrer"
    data-testid="chat-header-pr-badge"
    title={`Open ${labels.longSingular} ${label}`}
    class="shrink-0 inline-flex h-6 items-center rounded-[var(--radius-control)]
           px-2 text-[0.6875rem] font-medium tabular-nums text-text-secondary
           transition-[color,background-color] duration-150
           hover:bg-surface-2/40 hover:text-text-primary
           focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40"
  >
    {label}
  </a>
{/if}

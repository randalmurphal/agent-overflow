<script lang="ts">
  // Branch PR/MR badge for the chat header. Rendered only when the workspace
  // branch has an open pull/merge request (status.openPrUrl). The label uses
  // the forge-correct noun + sigil (PR #123 on GitHub, MR !123 on GitLab).
  //
  // A plain left-click prefers the in-app review: when `onOpenReview` is
  // provided and reports it handled the click, the default is prevented and
  // the companion review pane owns the PR. Everything else — mod+click,
  // middle-click, or no review target — falls through to the real <a href>,
  // where the app-wide external-link delegate (utils/externalLinks, installed
  // once in App.svelte) intercepts the click and routes it through
  // window.open (remote / --connect) or the OpenExternalURL host binding
  // (loopback / native, incl. WSL → Windows). safeExternalURL gates
  // rendering, mirroring the delegate's own scheme/host validation.
  import type { GitStatus } from '../../types/git';
  import { forgeLabels } from '../../utils/forgeLabels';
  import { safeExternalURL } from '../../utils/externalLinks';

  interface Props {
    status: GitStatus | null;
    /** Open the companion review pane on the PR. Returns whether it could;
     *  false falls back to opening the forge URL externally. */
    onOpenReview?: () => boolean;
  }

  let { status, onOpenReview }: Props = $props();

  let href = $derived(safeExternalURL(status?.openPrUrl));
  let labels = $derived(forgeLabels(status?.forge));
  let label = $derived(
    status?.openPrNumber
      ? `${labels.noun} ${labels.numberSigil}${status.openPrNumber}`
      : labels.noun,
  );

  function handleClick(event: MouseEvent): void {
    if (event.button !== 0) return;
    // Any modifier keeps the external-link behavior (mod+click opens the
    // forge page in the browser).
    if (event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;
    if (!onOpenReview?.()) return;
    event.preventDefault();
  }
</script>

{#if href}
  <a
    {href}
    target="_blank"
    rel="noopener noreferrer"
    data-testid="chat-header-pr-badge"
    title={`Open ${labels.longSingular} ${label}`}
    onclick={handleClick}
    class="shrink-0 inline-flex h-6 items-center rounded-[var(--radius-control)]
           px-2 text-[0.6875rem] font-medium tabular-nums text-text-secondary
           underline underline-offset-2 decoration-text-secondary/40
           transition-[color,background-color] duration-150
           hover:bg-surface-2/40 hover:text-text-primary hover:decoration-text-primary/50
           focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40"
  >
    {label}
  </a>
{/if}

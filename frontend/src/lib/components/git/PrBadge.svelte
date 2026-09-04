<script lang="ts">
  // Branch PR/MR badge for the chat header. Rendered only when the workspace
  // branch has an open pull/merge request (status.openPrUrl). The label uses
  // the forge-correct noun + sigil (PR #123 on GitHub, MR !123 on GitLab).
  //
  // A plain left-click prefers the in-app review: when `onOpenReview` is
  // provided and reports it handled the click, the default is prevented and
  // the companion review pane owns the PR. Mod+click is this badge's promise
  // of the FORGE PAGE in the person's real browser, so it is handled HERE
  // (handleExternalURL) rather than left to the app-wide delegate — that
  // delegate routes a mod+click into the thread's companion browser, which
  // for this control took away the only gesture that reached the system
  // browser. Middle-click and no-review-target still fall through to the
  // real <a href>, where the delegate opens externally. safeExternalURL
  // gates rendering, mirroring the delegate's own scheme/host validation.
  import type { GitStatus } from '../../types/git';
  import { forgeLabels } from '../../utils/forgeLabels';
  import { handleExternalURL, safeExternalURL } from '../../utils/externalLinks';
  import { isModClick } from '../../utils/modClick';
  import { isMacPlatform } from '../../utils/platform';

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
  const modLabel = isMacPlatform() ? '⌘' : 'Ctrl';
  let title = $derived(
    onOpenReview
      ? `Open the review pane · ${modLabel}+click opens the ${labels.longSingular.toLowerCase()} in your browser`
      : `Open ${labels.longSingular} ${label}`,
  );

  function handleClick(event: MouseEvent): void {
    if (event.button !== 0) return;
    if (isModClick(event)) {
      // Claim the gesture before the app-wide delegate turns it into a
      // companion-browser page: mod+click here means the system browser.
      event.preventDefault();
      if (href) void handleExternalURL(href);
      return;
    }
    // Other modifiers keep the anchor's external-link behavior.
    if (event.shiftKey || event.altKey) return;
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
    {title}
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

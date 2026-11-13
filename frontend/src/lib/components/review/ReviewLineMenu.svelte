<script lang="ts">
  import ContextMenu from '../primitives/ContextMenu.svelte';
  import MenuItem from '../primitives/MenuItem.svelte';
  import { addToast } from '../../stores/toast.svelte';
  import { copyToClipboard } from '../../utils/clipboard';
  import { reportFrontendDiagnostic } from '../../utils/frontendErrorCapture';
  import type { ReviewFile } from '../../utils/patchStore';
  import { lineLink, linePositionAt, type LineLinkForge } from '../../utils/reviewLineLink';

  // A diff line's number gutter menu in a pull request's diff: `Copy link
  // to line`, the line's address on the forge's own diff page
  // (utils/reviewLineLink.ts). One per review pane; a gutter's right-click
  // (a long press on compact, through the shared bridge) opens it at the
  // pointer. The link is worked out when the menu opens, so the copy runs
  // inside the click that asked for it.

  interface Props {
    forge: LineLinkForge;
    /** The PR's web address. */
    prUrl: string;
  }

  let { forge, prUrl }: Props = $props();

  let menu = $state<{ x: number; y: number; link: Promise<string> } | null>(null);

  /** Opens the menu for line `lineIndex` of `file`; false when the line is
   * not a row of it. */
  export function open(event: MouseEvent, file: ReviewFile, lineIndex: number): boolean {
    const at = linePositionAt(file, lineIndex);
    if (!at) return false;
    const link = lineLink(forge, prUrl, file.path, at);
    // A failure is reported where it is awaited.
    link.catch(() => {});
    menu = { x: event.clientX, y: event.clientY, link };
    return true;
  }

  function dismiss(): void {
    menu = null;
  }

  async function copyLink(event: MouseEvent | undefined): Promise<void> {
    const open = menu;
    dismiss();
    if (!open) return;
    let link: string;
    try {
      link = await open.link;
    } catch (err) {
      reportFrontendDiagnostic('review diff: a line link could not be made', err instanceof Error ? err.message : String(err));
      addToast('error', 'Could not make a link to this line.');
      return;
    }
    // copyToClipboard reports a refusal itself (reportCopyFailure).
    const copied = await copyToClipboard(link, event);
    addToast(copied ? 'info' : 'error', copied ? 'Copied link to line.' : 'Copy failed.');
  }
</script>

{#if menu}
  <ContextMenu x={menu.x} y={menu.y} ariaLabel="Line actions" onDismiss={dismiss} minWidthClass="min-w-[168px]">
    <MenuItem label="Copy link to line" testId="review-copy-line-link" onSelect={() => { void copyLink(undefined); }} />
  </ContextMenu>
{/if}

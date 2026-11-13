<script lang="ts">
  import ContextMenu from '../primitives/ContextMenu.svelte';
  import MenuItem from '../primitives/MenuItem.svelte';
  import { addToast } from '../../stores/toast.svelte';
  import { copyToClipboard } from '../../utils/clipboard';
  import { reportFrontendDiagnostic } from '../../utils/frontendErrorCapture';
  import type { ReviewFile } from '../../utils/patchStore';
  import { lineLink, linePositionAt, type LineLinkForge } from '../../utils/reviewLineLink';

  // A diff line's number gutter menu in a pull request's diff: a link to
  // the line on the forge's diff page (utils/reviewLineLink.ts). One per review pane;
  // a gutter's right-click (a long press on compact, through the shared
  // bridge) opens it at the pointer. The link is worked out when the menu
  // opens, so the copy runs inside the click that asked for it.

  interface Props {
    forge: LineLinkForge;
    /** The PR's web address. */
    prUrl: string;
  }

  let { forge, prUrl }: Props = $props();

  let menu = $state<{ x: number; y: number; link: string | Error } | null>(null);

  /** Opens the menu for line `lineIndex` of `file`; false (the native
   * menu stays) when no row of the file is that line. */
  export function open(event: MouseEvent, file: ReviewFile, lineIndex: number): boolean {
    const at = linePositionAt(file, lineIndex);
    if (!at) return false;
    let link: string | Error;
    try {
      link = lineLink(forge, prUrl, file.path, at);
    } catch (err) {
      // Reported when the item is chosen.
      link = err instanceof Error ? err : new Error(String(err));
    }
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
    if (open.link instanceof Error) {
      reportFrontendDiagnostic('review diff: a line link could not be made', open.link.message);
      addToast('error', 'Could not make a link to this line.');
      return;
    }
    // copyToClipboard reports a refusal itself (reportCopyFailure).
    const copied = await copyToClipboard(open.link, event);
    addToast(copied ? 'info' : 'error', copied ? 'Copied link.' : 'Copy failed.');
  }
</script>

{#if menu}
  <ContextMenu x={menu.x} y={menu.y} ariaLabel="Line actions" onDismiss={dismiss} minWidthClass="min-w-[168px]">
    <MenuItem label="Copy link to line" testId="review-copy-line-link" onSelect={() => { void copyLink(undefined); }} />
  </ContextMenu>
{/if}

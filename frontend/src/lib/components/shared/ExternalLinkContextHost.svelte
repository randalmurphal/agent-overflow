<script lang="ts">
  /**
   * Right-click menu for outbound links anywhere in the app.
   *
   * Mounted once from `App.svelte` next to `DiagramInteractionHost`, and
   * built the same way: one delegated `contextmenu` listener on `document`
   * filters for a link the click delegate would open, so no surface
   * allocates per-link handlers. State is three scalars plus the resolved
   * URL string.
   *
   * Left-click opening stays where it was — `installExternalLinkDelegate`
   * in `utils/externalLinks.ts`; both resolve the target through the same
   * `externalURLForEventTarget`, so the menu can never appear on something
   * the click path would not open.
   */

  import { onMount } from 'svelte';
  import ContextMenu from '../primitives/ContextMenu.svelte';
  import MenuItem from '../primitives/MenuItem.svelte';
  import { externalURLForEventTarget, handleExternalURL } from '../../utils/externalLinks';
  import { copyToClipboard } from '../../utils/clipboard';
  import { addToast } from '../../stores/toast.svelte';

  let menu: { x: number; y: number; url: string } | null = $state(null);

  function handleContextMenu(event: MouseEvent): void {
    if (event.defaultPrevented) return;
    const url = externalURLForEventTarget(event.target);
    if (!url) return;
    event.preventDefault();
    menu = { x: event.clientX, y: event.clientY, url };
  }

  function dismiss(): void {
    menu = null;
  }

  function openLink(): void {
    if (!menu) return;
    const url = menu.url;
    dismiss();
    void handleExternalURL(url);
  }

  async function copyLink(): Promise<void> {
    if (!menu) return;
    const url = menu.url;
    dismiss();
    const ok = await copyToClipboard(url);
    addToast(ok ? 'info' : 'error', ok ? 'Copied link address.' : 'Copy failed.');
  }

  onMount(() => {
    document.addEventListener('contextmenu', handleContextMenu);
    return () => document.removeEventListener('contextmenu', handleContextMenu);
  });
</script>

{#if menu}
  <ContextMenu
    x={menu.x}
    y={menu.y}
    ariaLabel="Link Actions"
    onDismiss={dismiss}
    minWidthClass="min-w-[168px]"
  >
    <MenuItem label="Open Link" onSelect={openLink} />
    <MenuItem label="Copy Link Address" onSelect={() => void copyLink()} />
  </ContextMenu>
{/if}

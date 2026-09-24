<script lang="ts">
  /**
   * Right-click (long-press on the phone) menu for images: Copy Image and
   * Save Image, on a thread's image attachments and on forge images in PR
   * and MR bodies and comments (`utils/imageMenuActions.ts`).
   *
   * Mounted once from `App.svelte`, built like `DiagramInteractionHost`:
   * one delegated `contextmenu` listener on `document` finds an element
   * tagged by `imageMenuActions`, so no image allocates a handler. State is
   * the pointer position, the image target and two elements.
   *
   * The lightbox is a dialog under this menu, and dismissing the menu must
   * leave it open. Escape is claimed by `ContextMenu`. A press outside the
   * menu dismisses it, and the click that press produces is kept from the
   * dialog, whose backdrop would otherwise close on it. Focus returns to
   * where it was when the menu took it, so the dialog's own keys (Escape,
   * arrows) keep working afterwards.
   */

  import { onMount, tick } from 'svelte';
  import ContextMenu from '../primitives/ContextMenu.svelte';
  import MenuItem from '../primitives/MenuItem.svelte';
  import { addToast } from '../../stores/toast.svelte';
  import { errString } from '../../utils/errors';
  import {
    canSaveMenuImage,
    copyMenuImage,
    saveMenuImage,
    saveMenuImageLabel,
    taggedMenuImage,
    type ImageMenuTarget,
  } from '../../utils/imageMenuActions';

  type MenuState = {
    x: number;
    y: number;
    target: ImageMenuTarget;
    /** The dialog the image sits in, when it sits in one. */
    dialog: Element | null;
    /** Where focus was when the menu opened. */
    returnFocus: HTMLElement | null;
  };

  let menu = $state<MenuState | null>(null);
  const saveGranted = $derived(menu ? canSaveMenuImage(menu.target) : true);
  const saveLabel = $derived(menu ? saveMenuImageLabel(menu.target) : 'Save Image');

  // Set by a press outside a menu raised over a dialog; spent by the click
  // that press produces.
  let shieldNextClick = false;

  function handleContextMenu(event: MouseEvent): void {
    // Something nearer the target already opened its menu.
    if (event.defaultPrevented) return;
    const tagged = taggedMenuImage(event.target);
    if (!tagged) return;
    event.preventDefault();
    const active = document.activeElement;
    menu = {
      x: event.clientX,
      y: event.clientY,
      target: tagged.target,
      dialog: tagged.element.closest('[role="dialog"]'),
      returnFocus: active instanceof HTMLElement && active !== document.body ? active : null,
    };
  }

  function close(): void {
    const closing = menu;
    if (!closing) return;
    menu = null;
    // The focused menu item leaves the document with the menu, which drops
    // focus to <body>. Put it back unless the dismissing press moved it.
    void tick().then(() => {
      const target = closing.returnFocus;
      const active = document.activeElement;
      if (!target?.isConnected) return;
      if (active !== null && active !== document.body) return;
      target.focus({ preventScroll: true });
    });
  }

  // Report a copy's outcome. It takes the in-flight promise rather than a
  // callback so the clipboard call is made at the call site, inside the
  // click's gesture task (utils/pngClipboard.ts note 2).
  async function report(copy: Promise<void>): Promise<void> {
    try {
      await copy;
      addToast('success', 'Image copied');
    } catch (err) {
      addToast('error', errString(err));
    }
  }

  function copyImage(): void {
    if (!menu) return;
    const { target } = menu;
    void report(copyMenuImage(target));
    close();
  }

  function saveImage(): void {
    if (!menu) return;
    const { target } = menu;
    close();
    void saveMenuImage(target);
  }

  // One press raises `pointerdown` and then, for a mouse, `mousedown`; by
  // the second the menu is already gone, so only the first decides.
  let pointerPressSeen = false;

  function handlePointerDown(event: PointerEvent): void {
    pointerPressSeen = true;
    judgePress(event);
  }

  function handleMouseDown(event: MouseEvent): void {
    if (pointerPressSeen) {
      pointerPressSeen = false;
      return;
    }
    judgePress(event);
  }

  function judgePress(event: MouseEvent): void {
    shieldNextClick = false;
    if (!menu?.dialog || event.button !== 0) return;
    if (event.target instanceof Element && event.target.closest('[data-context-menu]')) return;
    shieldNextClick = true;
  }

  function handleClick(event: MouseEvent): void {
    if (!shieldNextClick) return;
    shieldNextClick = false;
    event.stopImmediatePropagation();
    event.preventDefault();
  }

  function clearShield(): void {
    shieldNextClick = false;
  }

  onMount(() => {
    // Window capture runs ahead of ContextMenu's document-capture dismissal
    // and of every handler in the dialog.
    const capture = { capture: true } as const;
    document.addEventListener('contextmenu', handleContextMenu);
    window.addEventListener('pointerdown', handlePointerDown, capture);
    window.addEventListener('mousedown', handleMouseDown, capture);
    window.addEventListener('click', handleClick, capture);
    window.addEventListener('keydown', clearShield, capture);
    return () => {
      document.removeEventListener('contextmenu', handleContextMenu);
      window.removeEventListener('pointerdown', handlePointerDown, capture);
      window.removeEventListener('mousedown', handleMouseDown, capture);
      window.removeEventListener('click', handleClick, capture);
      window.removeEventListener('keydown', clearShield, capture);
    };
  });
</script>

{#if menu}
  <ContextMenu
    x={menu.x}
    y={menu.y}
    ariaLabel="Image Actions"
    onDismiss={close}
    minWidthClass="min-w-[168px]"
  >
    <MenuItem label="Copy Image" onSelect={copyImage} />
    <MenuItem
      label={saveLabel}
      disabled={!saveGranted}
      title={saveGranted ? undefined : 'Not granted to this device'}
      onSelect={saveImage}
    />
  </ContextMenu>
{/if}

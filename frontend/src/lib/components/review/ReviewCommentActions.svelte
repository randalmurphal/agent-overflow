<script lang="ts" module>
  import type { Component } from 'svelte';
  import type { LucideProps } from '@lucide/svelte';

  /** One action of a comment's header cluster. */
  export interface CommentAction {
    id: string;
    icon: Component<LucideProps>;
    label: string;
    run: () => unknown;
    /** Asked in place before it runs (`Delete?  Yes  Cancel`). */
    confirm?: string;
    disabled?: boolean;
    /** Why it is disabled, as its hover text. */
    disabledLabel?: string;
    testid?: string;
    /** It opens a popover (the host's `popover`, anchored to it). */
    haspopup?: 'dialog' | 'menu';
    /** Its popover is open: the icons stay shown while it is. */
    expanded?: boolean;
  }
</script>

<script lang="ts">
  import type { Snippet } from 'svelte';
  import { SvelteMap } from 'svelte/reactivity';
  import Ellipsis from '@lucide/svelte/icons/ellipsis';
  import Menu from '../primitives/Menu.svelte';
  import Popover from '../primitives/Popover.svelte';
  import Icon from '../primitives/Icon.svelte';
  import MenuItem from '../primitives/MenuItem.svelte';
  import ReviewIconButton from './ReviewIconButton.svelte';
  import { isCompactLayout } from '../../stores/layoutMode.svelte';
  import { restorePickerFocus } from '../panes/paneComposerFocus';
  import type { PopoverCloseReason } from '../../utils/popoverOwnership';

  // A comment's actions at the right end of its header. On desktop they
  // are icons revealed while the comment (header or body) is hovered, or
  // on focus; the icons hold their width while hidden. On compact, which
  // has no hover and no room for four icons beside the chips, they are one
  // `⋯` that opens the shared menu with the same rows in the same order,
  // and a long press on the comment (utils/longPressContextMenu.ts raises
  // it) opens the same menu at the `⋯`. The menu is a Popover, which
  // lifts it out of the card's clipping. A popover an action opens
  // anchors to its icon, or to the `⋯` on compact. The in-place confirm
  // lays over the header from the right, so neither revealing nor
  // confirming moves the header's text. Which action awaits its confirm
  // is the host's (a virtualized row can unmount mid-question).

  interface Props {
    actions: readonly CommentAction[];
    /** The id of the action asking its confirm, or null. */
    confirming: string | null;
    onConfirmingChange: (id: string | null) => void;
    /** A confirmed action is running or waiting for the forge's answer. */
    pending?: boolean;
    /** What hovering reveals the icons: the comment (a `group/comment`
     * ancestor), or a thread's first comment, whose header and body are
     * apart (a `data-first-comment` element inside `group/thread`). */
    reveal?: 'comment' | 'thread';
    /** An action's popover, given the button it anchors to. */
    popover?: Snippet<[anchor: (id: string) => HTMLButtonElement | undefined]>;
  }

  let { actions, confirming, onConfirmingChange, pending = false, reveal = 'comment', popover }: Props = $props();
  const buttons = new SvelteMap<string, HTMLButtonElement>();
  const compact = $derived(isCompactLayout());
  let root: HTMLSpanElement | undefined = $state(undefined);
  let trigger: HTMLButtonElement | undefined = $state(undefined);
  /** The compact menu is open. */
  let menuOpen = $state(false);
  const held = $derived(actions.some((action) => action.expanded));
  const REVEAL = {
    comment: 'group-hover/comment:opacity-100',
    thread: 'group-has-[[data-first-comment]:hover]/thread:opacity-100',
  } as const;

  const asking = $derived(actions.find((action) => action.id === confirming) ?? null);

  function press(action: CommentAction): void {
    if (action.confirm) onConfirmingChange(action.id);
    else void action.run();
  }

  function anchor(id: string): HTMLButtonElement | undefined {
    return compact ? trigger : buttons.get(id);
  }

  function closeMenu(reason?: PopoverCloseReason): void {
    menuOpen = false;
    restorePickerFocus(reason, { triggerEl: trigger });
  }

  // A long press on the comment opens the menu: on a reply, anywhere in
  // its `group/comment`; on a thread's first comment, its header or body
  // (`data-first-comment`), not a reply's. A link keeps the engine's own
  // menu.
  $effect(() => {
    const host = root;
    if (!compact || !host || actions.length === 0) return;
    const scope = host.closest(reveal === 'comment' ? '.group\\/comment' : '.group\\/thread');
    if (!(scope instanceof HTMLElement)) return;
    const onContextMenu = (event: MouseEvent): void => {
      const target = event.target;
      if (!(target instanceof Element) || target.closest('a[href]')) return;
      if (reveal === 'thread' && (!target.closest('[data-first-comment]') || target.closest('.group\\/comment'))) return;
      event.preventDefault();
      event.stopPropagation();
      menuOpen = true;
    };
    scope.addEventListener('contextmenu', onContextMenu);
    return () => scope.removeEventListener('contextmenu', onContextMenu);
  });

  const textButton = 'h-6 rounded-[var(--radius-control)] px-1.5 text-[0.6875rem] compact:h-8 compact:px-2.5 disabled:opacity-45';
</script>

{#if actions.length > 0}
  <span bind:this={root} class="relative flex shrink-0 items-center" data-testid="review-comment-actions" data-find-skip>
    {#if compact}
      <span class="flex items-center {asking ? 'invisible' : ''}">
        <ReviewIconButton
          icon={Ellipsis}
          label="Comment actions"
          haspopup="menu"
          expanded={menuOpen}
          disabled={pending}
          testid="review-comment-menu"
          bind:element={trigger}
          onclick={() => { menuOpen = !menuOpen; }}
        />
      </span>
      <Popover anchor={trigger} open={menuOpen} onClose={closeMenu} placement="bottom-end" role="none">
        <Menu ariaLabel="Comment actions" onClose={closeMenu} minWidthClass="min-w-[160px]">
          {#each actions as action (action.id)}
            <MenuItem
              label={action.label}
              disabled={action.disabled || pending}
              title={action.disabled ? action.disabledLabel : undefined}
              testId={action.testid}
              variant={action.id === 'delete' ? 'danger' : 'default'}
              onSelect={() => { menuOpen = false; press(action); }}
            >
              {#snippet icon()}<Icon icon={action.icon} size={14} />{/snippet}
            </MenuItem>
          {/each}
        </Menu>
      </Popover>
    {:else}
    <span
      class="flex items-center opacity-0 transition-opacity duration-75 {REVEAL[reveal]} focus-within:opacity-100 {held ? 'opacity-100' : ''} {asking ? 'invisible' : ''}"
    >
      {#each actions as action (action.id)}
        <ReviewIconButton
          icon={action.icon}
          label={action.label}
          disabled={action.disabled || pending}
          disabledLabel={action.disabledLabel}
          testid={action.testid}
          haspopup={action.haspopup}
          expanded={action.expanded}
          bind:element={() => buttons.get(action.id), (element) => { if (element) buttons.set(action.id, element); else buttons.delete(action.id); }}
          onclick={() => press(action)}
        />
      {/each}
    </span>
    {/if}
    {@render popover?.(anchor)}
    {#if asking}
      <span
        class="absolute inset-y-0 right-0 flex items-center gap-1 whitespace-nowrap pl-2 text-[0.6875rem] text-fg"
        role="group"
        aria-label={asking.confirm}
        data-testid="review-comment-confirm"
      >
        <span class="text-fg-muted">{asking.confirm}</span>
        <button
          type="button"
          class="{textButton} font-medium text-error hover:bg-error/10"
          disabled={pending}
          data-testid="review-comment-confirm-yes"
          onclick={() => { void asking.run(); }}
        >Yes</button>
        <button
          type="button"
          class="{textButton} text-fg-muted hover:bg-surface-2 hover:text-fg"
          data-testid="review-comment-confirm-cancel"
          onclick={() => onConfirmingChange(null)}
        >Cancel</button>
      </span>
    {/if}
  </span>
{/if}

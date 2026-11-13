<script lang="ts">
  import Ellipsis from '@lucide/svelte/icons/ellipsis';
  import Menu from '../primitives/Menu.svelte';
  import MenuItem from '../primitives/MenuItem.svelte';
  import Popover from '../primitives/Popover.svelte';
  import ReviewIconButton from './ReviewIconButton.svelte';
  import { restorePickerFocus } from '../panes/paneComposerFocus';
  import { PR_MANAGE_ACTION_LABEL, type PRManageView } from '../../stores/prManage.svelte';
  import { PR_STATE_LABEL, prStateMenuItems } from '../../utils/prManage';
  import type { PopoverCloseReason } from '../../utils/popoverOwnership';
  import type { PRDetail } from '../../types/models';

  // The title bar's `⋯` at the end of the state line: Close or Reopen,
  // and Convert to draft on an open PR that is not one, each only when
  // the forge says the viewer may (manage.can). No confirm step: each is
  // undone by the item that replaces it. A merged PR has none, so no
  // menu. Revealed on hover like the other edit affordances, always
  // shown on compact, and shown while its action is pending.

  interface Props {
    manage: PRManageView;
    detail: PRDetail;
  }

  let { manage, detail }: Props = $props();

  const items = $derived(prStateMenuItems(detail));
  const pending = $derived(manage.pending);
  const statePending = $derived(pending !== null && (pending.kind === 'close' || pending.kind === 'reopen' || pending.kind === 'draft'));
  let open = $state(false);
  let trigger: HTMLButtonElement | undefined = $state(undefined);

  function close(reason?: PopoverCloseReason): void {
    open = false;
    restorePickerFocus(reason, { triggerEl: trigger });
  }
</script>

{#if items.length > 0}
  <span
    class="ml-auto inline-flex shrink-0 transition-opacity {open || statePending ? 'opacity-100' : 'opacity-0 focus-within:opacity-100 group-hover/titlebar:opacity-100 compact:opacity-100'}"
  >
    <ReviewIconButton
      bind:element={trigger}
      icon={Ellipsis}
      label="Pull request actions"
      testid="review-pr-state-menu"
      haspopup="menu"
      expanded={open}
      pending={statePending}
      focusableWhilePending
      disabled={manage.blockedReason !== null && !statePending}
      disabledLabel={statePending && pending ? `${PR_MANAGE_ACTION_LABEL[pending.kind]}…` : (manage.blockedReason ?? undefined)}
      onclick={() => { open = !open; }}
    />
  </span>
  <Popover anchor={trigger} {open} onClose={close} placement="bottom-end" role="none">
    <Menu ariaLabel="Pull request actions" onClose={close} minWidthClass="min-w-[180px]">
      <div data-testid="review-pr-state-items">
        {#each items as item (item)}
          <MenuItem
            label={PR_STATE_LABEL[item]}
            testId="review-pr-state-{item}"
            onSelect={() => {
              open = false;
              trigger?.focus({ preventScroll: true });
              void manage.setState(item);
            }}
          />
        {/each}
      </div>
    </Menu>
  </Popover>
{/if}

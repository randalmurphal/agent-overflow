<script lang="ts">
  // The compact layout's visible door into a row's menu. A row's menu opens
  // from right-click, and on the phone from a long press
  // (utils/longPressContextMenu.ts), but a hidden gesture is not an
  // affordance: everything a hold reaches has to be reachable by something
  // you can see. Desktop never renders it (`hidden compact:flex`); the hover
  // controls are its visible half there.
  //
  // It raises the SAME handler the row's `oncontextmenu` runs, so the two
  // doors cannot drift. Click and double-click stop here so the row under
  // it neither opens the thread nor starts a rename.
  import Ellipsis from '@lucide/svelte/icons/ellipsis';
  import Icon from '../primitives/Icon.svelte';

  interface Props {
    label: string;
    testId: string;
    onOpen: (e: MouseEvent) => void;
  }

  let { label, testId, onOpen }: Props = $props();
</script>

<button
  type="button"
  aria-label={label}
  title={label}
  data-testid={testId}
  onclick={(e) => {
    e.stopPropagation();
    onOpen(e);
  }}
  ondblclick={(e) => e.stopPropagation()}
  class="hidden compact:flex h-9 w-9 -mr-1 shrink-0 items-center justify-center rounded-[var(--radius-field)] text-fg-subtle active:bg-surface-2/40 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40"
>
  <Icon icon={Ellipsis} size={14} strokeWidth={2} class="opacity-90" />
</button>

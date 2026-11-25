<script lang="ts" module>
  import type { Snippet } from 'svelte';

  /** What an action's page draws in: Back to the menu, and close. */
  export interface ActionPageControls {
    back(): void;
    close(): void;
  }

  /** One row of an action menu. */
  export interface MenuAction {
    /** Stable across releases: the shown set is saved by it. */
    readonly id: string;
    readonly label: string;
    readonly description?: string;
    readonly disabled?: boolean;
    readonly testId?: string;
    /** Runs on choosing the action, then the menu closes. */
    readonly run?: () => void;
    /** Opens in place of the menu's rows, with a Back row, on choosing
     * the action (never on hover): a list a read fills, or a choice of
     * several. */
    readonly page?: Snippet<[ActionPageControls]>;
  }

  /** A group of the menu, under its header. */
  export interface MenuGroup {
    readonly label: string;
    readonly actions: readonly MenuAction[];
  }
</script>

<script lang="ts">
  // A right-click menu that shows some actions at once and the rest behind
  // Show more, the person choosing which (docs/specs/issues.md, "Board").
  //
  // A caller gives: the menu's name (one per kind of menu, such as
  // `issues-card`, which keys the saved choice), the actions that apply
  // now in their groups, each with a stable ID, and the IDs shown at once
  // until the person chooses (`defaults`). An action either runs (then the
  // menu closes) or opens a page drawn in the menu's place; `opening`
  // names one whose page the menu opens at. The caller leaves out an
  // action that does not apply; it keeps no other state.
  //
  // The menu keeps: the person's choice of what shows at once, per menu
  // name on this device (`stores/menuShownActions.svelte.ts`), changed from
  // "Choose what shows first…" at the end of Show more; and, while open,
  // whether Show more is open and which page shows. Hovering a row does
  // nothing but highlight it, so no page's read starts from a hover.
  import { tick } from 'svelte';
  import ContextMenu from '../primitives/ContextMenu.svelte';
  import MenuDivider from '../primitives/MenuDivider.svelte';
  import MenuItem from '../primitives/MenuItem.svelte';
  import MenuSectionHeader from '../primitives/MenuSectionHeader.svelte';
  import { setActionShown, shownActions } from '../../stores/menuShownActions.svelte';

  interface Props {
    /** Viewport coordinates the menu opens at. */
    x: number;
    y: number;
    ariaLabel: string;
    name: string;
    defaults: readonly string[];
    groups: readonly MenuGroup[];
    /** The ID of the action whose page shows on opening. */
    opening?: string;
    onDismiss: () => void;
  }

  let { x, y, ariaLabel, name, defaults, groups, opening, onDismiss }: Props = $props();

  type Mode = { readonly kind: 'menu'; readonly more: boolean } | { readonly kind: 'choose' } | { readonly kind: 'page'; readonly action: MenuAction };
  function openingMode(): Mode {
    const action = opening ? groups.flatMap((g) => g.actions).find((a) => a.id === opening && a.page && !a.disabled) : undefined;
    return action ? { kind: 'page', action } : { kind: 'menu', more: false };
  }
  // svelte-ignore state_referenced_locally
  let mode = $state.raw<Mode>(openingMode());
  let rowsEl: HTMLDivElement | undefined = $state();

  const shown = $derived(shownActions(name, defaults));
  const atOnce = $derived(groups.map((g) => ({ label: g.label, actions: g.actions.filter((a) => shown.has(a.id)) })).filter((g) => g.actions.length > 0));
  const rest = $derived(groups.map((g) => ({ label: g.label, actions: g.actions.filter((a) => !shown.has(a.id)) })).filter((g) => g.actions.length > 0));

  /** Moves to `next` and puts focus on its first row, as opening did. */
  async function go(next: Mode): Promise<void> {
    mode = next;
    await tick();
    rowsEl?.querySelector<HTMLElement>('[role="menuitem"]:not([aria-disabled="true"]), [role="menuitemcheckbox"]')?.focus();
  }

  function choose(action: MenuAction): void {
    if (action.page) {
      void go({ kind: 'page', action });
      return;
    }
    // Before the close, which may take the state the action reads.
    action.run?.();
    onDismiss();
  }

  const controls: ActionPageControls = {
    back: () => void go({ kind: 'menu', more: false }),
    close: () => onDismiss(),
  };
</script>

{#snippet rows(list: typeof atOnce)}
  {#each list as group, i (group.label)}
    {#if i > 0}<MenuDivider />{/if}
    <MenuSectionHeader label={group.label} />
    {#each group.actions as action (action.id)}
      <MenuItem label={action.label} description={action.description} disabled={action.disabled} testId={action.testId} onSelect={() => choose(action)} />
    {/each}
  {/each}
{/snippet}

<ContextMenu {x} {y} {ariaLabel} {onDismiss}>
  <div bind:this={rowsEl} class="contents" data-testid="action-menu" data-menu={name} data-mode={mode.kind}>
    {#if mode.kind === 'page'}
      <MenuItem label="Back" testId="action-menu-back" onSelect={controls.back} />
      <MenuDivider />
      {@render mode.action.page?.(controls)}
    {:else if mode.kind === 'choose'}
      <MenuSectionHeader label="Show at once" />
      {#each groups as group (group.label)}
        {#each group.actions as action (action.id)}
          <MenuItem label={action.label} checkbox checked={shown.has(action.id)} testId="action-menu-choose-item" onSelect={() => setActionShown(name, defaults, action.id, !shown.has(action.id))} />
        {/each}
      {/each}
      <MenuDivider />
      <MenuItem label="Done" testId="action-menu-choose-done" onSelect={() => void go({ kind: 'menu', more: true })} />
    {:else}
      {@render rows(atOnce)}
      {#if mode.more}
        {#if atOnce.length > 0 && rest.length > 0}<MenuDivider />{/if}
        {@render rows(rest)}
        <MenuDivider />
        <MenuItem label="Choose what shows first…" testId="action-menu-choose" onSelect={() => void go({ kind: 'choose' })} />
      {:else}
        <MenuDivider />
        <MenuItem label={rest.length > 0 ? 'Show more' : 'Choose what shows first…'} testId={rest.length > 0 ? 'action-menu-more' : 'action-menu-choose'} onSelect={() => void go(rest.length > 0 ? { kind: 'menu', more: true } : { kind: 'choose' })} />
      {/if}
    {/if}
  </div>
</ContextMenu>

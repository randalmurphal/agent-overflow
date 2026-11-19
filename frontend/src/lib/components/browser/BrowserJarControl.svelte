<script lang="ts">
  import Boxes from '@lucide/svelte/icons/boxes';
  import Cookie from '@lucide/svelte/icons/cookie';
  import SquareUser from '@lucide/svelte/icons/square-user';
  import VenetianMask from '@lucide/svelte/icons/venetian-mask';
  import { setBrowserJar, type BrowserJar } from '../../stores/browserActions';
  import { browserCompanionState } from '../../stores/browserCompanion.svelte';
  import type { PopoverCloseReason } from '../../utils/popoverOwnership';
  import { restorePickerFocus } from '../panes/paneComposerFocus';
  import Icon from '../primitives/Icon.svelte';
  import IconButton from '../primitives/IconButton.svelte';
  import Menu from '../primitives/Menu.svelte';
  import MenuItem from '../primitives/MenuItem.svelte';
  import Popover from '../primitives/Popover.svelte';
  import ConfirmDialog from '../shared/ConfirmDialog.svelte';
  import { browserPaneThread } from './browserPaneThread';

  // The browser's cookie jar: which threads' browsers share sign-ins and
  // site data with this one (internal/browser/jar.go). Switching with tabs
  // open closes them and suspends their addresses into the new jar. The
  // backend decides whether a tab is live in the hold that would switch: it
  // refuses a switch the user has not confirmed then, and this asks and
  // sends it again confirmed.

  const JARS: { kind: BrowserJar; label: string; description: string; icon: typeof Cookie }[] = [
    { kind: 'worktree', label: 'This worktree', description: 'Shared with every thread on this worktree', icon: Cookie },
    { kind: 'project', label: 'Whole project', description: "Shared with this project's threads that choose it, in any worktree", icon: Boxes },
    { kind: 'thread', label: 'This browser only', description: "This thread's own, kept between sessions", icon: SquareUser },
    { kind: 'private', label: 'Private', description: 'Not kept; discarded when its last tab closes', icon: VenetianMask },
  ];

  const thread = browserPaneThread();
  let threadId = $derived(thread());
  let browser = $derived(threadId ? browserCompanionState(threadId) : null);
  let current = $derived(JARS.find((jar) => jar.kind === browser?.jar) ?? JARS[0]);
  // The open tabs the confirmation names; whether to ask is the backend's.
  let openTabs = $derived((browser?.pages ?? []).filter((page) => !page.suspended && !page.error).length);

  let anchor: HTMLSpanElement | undefined = $state(undefined);
  // The element the menu was opened from instead of the trigger: the address
  // row's More button once the row is too narrow to show the trigger.
  let openedFrom: HTMLElement | undefined = $state(undefined);
  let open = $state(false);
  let confirming = $state<(typeof JARS)[number] | null>(null);

  /** Opens the jar menu anchored at `from` (the More button). */
  export function openMenu(from: HTMLElement | undefined): void {
    openedFrom = from;
    open = true;
  }

  /** The current jar's name, for the More menu's row; null outside a pane. */
  export function currentLabel(): string | null {
    return threadId ? current.label : null;
  }

  function trigger(): HTMLElement | undefined {
    return (openedFrom ?? anchor)?.querySelector<HTMLElement>('button') ?? undefined;
  }

  function closeMenu(reason?: PopoverCloseReason): void {
    const target = trigger();
    open = false;
    openedFrom = undefined;
    restorePickerFocus(reason, { triggerEl: target });
  }

  function choose(jar: (typeof JARS)[number]): void {
    // Focus goes back to what opened the menu before a confirm opens, so the
    // confirm returns it there.
    trigger()?.focus();
    open = false;
    openedFrom = undefined;
    if (!threadId || jar.kind === current.kind) return;
    void switchTo(threadId, jar);
  }

  async function switchTo(thread: string, jar: (typeof JARS)[number]): Promise<void> {
    const answer = await setBrowserJar(thread, jar.kind, false);
    if (answer.kind === 'needs-confirmation' && thread === threadId) confirming = jar;
  }

  function confirm(): void {
    const jar = confirming;
    confirming = null;
    if (threadId && jar) void setBrowserJar(threadId, jar.kind, true);
  }
</script>

{#if threadId}
  <span bind:this={anchor} class="inline-flex shrink-0">
    <IconButton
      label="Cookie jar: {current.label}"
      size="sm"
      testId="browser-jar"
      ariaHaspopup="menu"
      ariaExpanded={open}
      onClick={() => {
        openedFrom = undefined;
        open = !open;
      }}
    ><Icon icon={current.icon} size={14} /></IconButton>
  </span>
  <Popover anchor={openedFrom ?? anchor} {open} onClose={closeMenu} placement="bottom-end" role="none">
    <Menu ariaLabel="Cookie jar" minWidthClass="w-[min(19rem,calc(100vw-2rem))] min-w-0" onClose={closeMenu}>
      {#each JARS as jar (jar.kind)}
        <MenuItem
          label={jar.label}
          description={jar.description}
          checkbox
          checked={jar.kind === current.kind}
          testId="browser-jar-{jar.kind}"
          onSelect={() => choose(jar)}
        />
      {/each}
    </Menu>
  </Popover>
  <ConfirmDialog
    open={confirming !== null}
    title="Switch to {confirming?.label ?? ''}?"
    description="{openTabs === 1 ? 'The open tab closes and loads its address' : openTabs > 1 ? `The ${openTabs} open tabs close and load their addresses` : 'The open tabs close and load their addresses'} again in the new cookie jar when you go back to {openTabs === 1 ? 'it' : 'them'}. Page state is lost: form input, back and forward history, and sign-ins kept in the current jar."
    confirmLabel="Switch cookie jar"
    destructive
    onConfirm={confirm}
    onCancel={() => (confirming = null)}
  />
{/if}
